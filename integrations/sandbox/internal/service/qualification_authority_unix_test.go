//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

type authorityFixture struct {
	authority *QualificationAuthority
	cfg       QualificationAuthorityConfig
	policy    QualificationAuthorityPolicy
	record    QualificationRecord
	key       ed25519.PrivateKey
	request   QualificationRequest
	client    string
}

const passingProbe = "{\"Action\":\"start\",\"Package\":\"live/probes\"}\n{\"Action\":\"run\",\"Package\":\"live/probes\",\"Test\":\"TestLiveProbe\"}\n{\"Action\":\"pass\",\"Package\":\"live/probes\",\"Test\":\"TestLiveProbe\"}\n{\"Action\":\"pass\",\"Package\":\"live/probes\"}\n"

func newAuthorityFixture(t *testing.T) *authorityFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := QualificationRequest{SchemaVersion: 1, Source: "qualification:authority", Nonce: evidence.Digest([]byte("nonce")), Host: qualificationHostFixture(), Deployment: qualificationDeploymentFixture()}
	response := remoteQualificationFixture(request)
	record := QualificationRecord{SchemaVersion: 1, Kind: "live-systemd-runsc-v1", Source: request.Source, Reference: "probe:one", Host: request.Host, Qualification: response.Qualification, ObservedAt: time.Now().UTC()}
	for _, name := range qualificationChecks() {
		record.Probes = append(record.Probes, QualificationProbe{Name: name, Package: "live/probes", Test: "TestLiveProbe", Output: credentialPin(t, filepath.Join(root, name+".jsonl"), datedProbe(passingProbe, record.ObservedAt.Add(-time.Second)))})
	}
	f := &authorityFixture{record: record, key: private, request: request, client: evidence.Digest([]byte("client"))}
	f.cfg = QualificationAuthorityConfig{SchemaVersion: 1, Source: request.Source, Policy: filepath.Join(root, "policy.json"), MaxEvidenceAge: time.Hour, DecisionTTL: 30 * time.Second, Timeout: time.Second, Readers: 2}
	f.policy = QualificationAuthorityPolicy{SchemaVersion: 1, Source: request.Source, ValidUntil: time.Now().Add(time.Hour), Grants: []QualificationAuthorityGrant{{ClientSHA256: f.client, Host: request.Host, Deployment: request.Deployment, ProducerKey: public, Record: hostfs.Pin{Path: filepath.Join(root, "record.json")}}}}
	f.save(t)
	f.authority, err = newQualificationAuthority(f.cfg, uint32(os.Geteuid()), func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *authorityFixture) save(t *testing.T) {
	t.Helper()
	f.record.Signature = nil
	payload, err := json.Marshal(f.record)
	if err != nil {
		t.Fatal(err)
	}
	f.record.Signature = ed25519.Sign(f.key, append([]byte("cortex-sandbox-qualification-record-v1\n"), payload...))
	body, err := json.Marshal(f.record)
	if err != nil {
		t.Fatal(err)
	}
	f.policy.Grants[0].Record = credentialPin(t, f.policy.Grants[0].Record.Path, body)
	writeInstalledPolicy(t, f.cfg.Policy, f.policy)
}
func TestQualificationAuthorityRetainsAndRechecks(t *testing.T) {
	f := newAuthorityFixture(t)
	got, err := f.authority.evaluate(t.Context(), f.request, f.client)
	if err != nil || got.Nonce != f.request.Nonce || got.Evidence.SHA256 != f.policy.Grants[0].Record.SHA256 {
		t.Fatal("retained evidence refused", err)
	}
	client := QualificationClient{cfg: QualificationClientConfig{MaxEvidenceAge: time.Hour}}
	if !client.validResponse(f.request, got, time.Now().Add(-time.Second), time.Now()) {
		t.Fatal("authority response is incompatible")
	}
	restarted, err := newQualificationAuthority(f.cfg, uint32(os.Geteuid()), func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.evaluate(t.Context(), f.request, f.client); err != nil {
		t.Fatal(err)
	}
	f.policy.Grants = nil
	writeInstalledPolicy(t, f.cfg.Policy, f.policy)
	if _, err = f.authority.evaluate(t.Context(), f.request, f.client); err == nil {
		t.Fatal("revoked grant reused")
	}
}
func TestQualificationAuthorityRejectsUntrustedEvidence(t *testing.T) {
	for _, mode := range []string{"signature", "fixture", "missing-probe", "duplicate-probe", "stale", "future", "boot", "deployment", "failed-log", "skip-log", "output-spoof", "missing-package-pass", "tampered-log", "wrong-client", "expired-policy", "changed-policy", "cancelled", "stale-log", "future-log"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuthorityFixture(t)
			switch mode {
			case "fixture":
				f.record.Kind = "fixture"
			case "missing-probe":
				f.record.Probes = f.record.Probes[1:]
			case "duplicate-probe":
				f.record.Probes[1] = f.record.Probes[0]
			case "stale":
				f.record.ObservedAt = time.Now().Add(-2 * time.Hour)
			case "future":
				f.record.ObservedAt = time.Now().Add(time.Hour)
			case "boot":
				f.record.Host.BootID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
			case "deployment":
				f.record.Qualification.Deployment.Broker = "other"
			case "expired-policy":
				f.policy.ValidUntil = time.Now().Add(-time.Second)
			}
			if mode == "failed-log" || mode == "skip-log" || mode == "output-spoof" || mode == "missing-package-pass" {
				output := []byte("{\"Action\":\"fail\",\"Package\":\"live/probes\",\"Test\":\"TestLiveProbe\"}\n")
				if mode == "skip-log" {
					output = []byte("{\"Action\":\"skip\",\"Package\":\"live/probes\",\"Test\":\"TestLiveProbe\"}\n")
				}
				if mode == "output-spoof" {
					output = []byte("{\"Action\":\"output\",\"Package\":\"live/probes\",\"Output\":\"PASS TestLiveProbe\"}\n")
				}
				if mode == "missing-package-pass" {
					output = []byte("{\"Action\":\"pass\",\"Package\":\"live/probes\",\"Test\":\"TestLiveProbe\"}\n")
				}
				output = append([]byte(strings.Join(strings.Split(passingProbe, "\n")[:2], "\n")+"\n"), output...)
				f.record.Probes[0].Output = credentialPin(t, f.record.Probes[0].Output.Path, datedProbe(string(output), f.record.ObservedAt.Add(-time.Second)))
			}
			if mode == "stale-log" || mode == "future-log" {
				observed := f.record.ObservedAt.Add(-2 * time.Hour)
				if mode == "future-log" {
					observed = f.record.ObservedAt.Add(time.Hour)
				}
				f.record.Probes[0].Output = credentialPin(t, f.record.Probes[0].Output.Path, datedProbe(passingProbe, observed))
			}
			f.save(t)
			if mode == "signature" {
				f.record.Signature[0] ^= 1
				body, _ := json.Marshal(f.record)
				f.policy.Grants[0].Record = credentialPin(t, f.policy.Grants[0].Record.Path, body)
				writeInstalledPolicy(t, f.cfg.Policy, f.policy)
			}
			if mode == "tampered-log" {
				if err := os.WriteFile(f.record.Probes[0].Output.Path, []byte("PASS"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "wrong-client" {
				f.client = evidence.Digest([]byte("other"))
			}
			if mode == "changed-policy" {
				f.authority.trust = func(context.Context) error {
					f.policy.Grants = nil
					writeInstalledPolicy(t, f.cfg.Policy, f.policy)
					return nil
				}
			}
			ctx := t.Context()
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := f.authority.evaluate(ctx, f.request, f.client); err == nil {
				t.Fatal("untrusted evidence accepted")
			}
		})
	}
}

func TestQualificationProbeRejectsIncompleteSubtests(t *testing.T) {
	probe := QualificationProbe{Package: "live/probes", Test: "TestLiveProbe"}
	for _, mode := range []string{"unstarted-pass", "unfinished-run", "unrelated-run", "duplicate-key", "trailing-object"} {
		t.Run(mode, func(t *testing.T) {
			extra := "{\"Action\":\"pass\",\"Package\":\"live/probes\",\"Test\":\"TestLiveProbe/sub\"}\n"
			switch mode {
			case "unfinished-run":
				extra = "{\"Action\":\"run\",\"Package\":\"live/probes\",\"Test\":\"TestLiveProbe/sub\"}\n"
			case "unrelated-run":
				extra = "{\"Action\":\"run\",\"Package\":\"live/probes\",\"Test\":\"TestUnitFixture\"}\n"
			case "duplicate-key":
				extra = "{\"Action\":\"fail\",\"Action\":\"output\",\"Package\":\"live/probes\"}\n"
			case "trailing-object":
				extra = "{\"Action\":\"output\",\"Package\":\"live/probes\"}{}\n"
			}
			prefix := "{\"Action\":\"start\",\"Package\":\"live/probes\"}\n"
			body := strings.Replace(passingProbe, prefix, prefix+extra, 1)
			if _, passed := passedQualificationProbe(datedProbe(body, time.Now().Add(-time.Second)), probe, time.Now(), time.Hour); passed {
				t.Fatal("incomplete probe accepted")
			}
		})
	}
}

func TestQualificationAuthorityHTTPBounds(t *testing.T) {
	for _, mode := range []string{"accepted", "plaintext", "wrong-certificate", "method", "query", "duplicate-json", "oversized", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuthorityFixture(t)
			body, _ := json.Marshal(f.request)
			if mode == "duplicate-json" {
				body = append([]byte(`{"schema_version":1,`), body[1:]...)
			}
			if mode == "oversized" {
				body = bytes.Repeat([]byte("x"), qualificationResponseBytes+1)
			}
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://localhost/v1/qualification", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			certificate := &x509.Certificate{Raw: []byte("client")}
			request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}, PeerCertificates: []*x509.Certificate{certificate}}
			switch mode {
			case "plaintext":
				request.TLS = nil
			case "wrong-certificate":
				certificate.Raw = []byte("other")
			case "method":
				request.Method = http.MethodGet
			case "query":
				request.URL.RawQuery = "override=1"
			case "capacity":
				for range f.cfg.Readers {
					f.authority.slots <- struct{}{}
				}
			}
			result := httptest.NewRecorder()
			f.authority.ServeHTTP(result, request)
			want := http.StatusForbidden
			if mode == "accepted" {
				want = http.StatusOK
			}
			if mode == "capacity" {
				want = http.StatusServiceUnavailable
			}
			if result.Code != want {
				t.Fatalf("status=%d want=%d", result.Code, want)
			}
			if result.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cacheable evidence")
			}
			if mode != "accepted" && strings.Contains(result.Body.String(), f.cfg.Policy) {
				t.Fatal("private path disclosed")
			}
		})
	}
}

func TestQualificationAuthorityInstalledTLSRoundTrip(t *testing.T) {
	f := newAuthorityFixture(t)
	credentials := daemonCredentialsFixture(t)
	certificate, err := tls.X509KeyPair(credentials.certificate, credentials.tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair(credentials.clientCertificate, credentials.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	f.client = evidence.Digest(clientCert.Certificate[0])
	f.policy.Grants[0].ClientSHA256 = f.client
	f.save(t)
	serverCfg := QualificationAuthorityServerConfig{SchemaVersion: 1, Authority: f.cfg, Listen: "127.0.0.1:8099", Certificate: credentials.files.Certificate, TLSKey: credentials.files.TLSKey, ClientCAs: credentials.files.ClientCAs, Limits: broker.ServerLimits{Connections: 4, HeaderBytes: 4096, HeaderTimeout: time.Second, RequestTimeout: f.cfg.Timeout, IdleTimeout: time.Second}}
	name := filepath.Join(filepath.Dir(f.cfg.Policy), "authority.json")
	mismatch := serverCfg
	mismatch.Limits.RequestTimeout = 3 * time.Second
	if validQualificationAuthorityServer(name, mismatch) {
		t.Fatal("transport timeout exceeds authority budget")
	}
	writeInstalledPolicy(t, name, serverCfg)
	for attempt := range 2 {
		authority, server, _, loadErr := readQualificationAuthority(t.Context(), name, uint32(os.Geteuid()))
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		listener, listenErr := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if listenErr != nil {
			t.Fatal(listenErr)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- server.ServeHandler(ctx, listener, authority) }()
		t.Cleanup(func() { cancel() })
		root := filepath.Dir(f.cfg.Policy)
		cfg := QualificationClientConfig{SchemaVersion: 1, Endpoint: strings.Replace("https://"+listener.Addr().String(), "127.0.0.1", "localhost", 1) + "/v1/qualification", Source: f.cfg.Source, ServerSPKI: evidence.Digest(leaf.RawSubjectPublicKeyInfo), Certificate: credentialPin(t, filepath.Join(root, "client.pem"), credentials.clientCertificate), TLSKey: credentialPin(t, filepath.Join(root, "client-key.pem"), credentials.clientKey), ServerCAs: credentials.files.ClientCAs, Timeout: time.Second, MaxEvidenceAge: time.Hour, Readers: 2}
		clientPath := filepath.Join(root, "client.json")
		writeInstalledPolicy(t, clientPath, cfg)
		client, clientErr := readQualificationClient(t.Context(), clientPath, uint32(os.Geteuid()), func(context.Context) (QualificationHost, error) { return f.request.Host, nil })
		if clientErr != nil {
			t.Fatal(clientErr)
		}
		if attempt == 0 {
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(credentials.ca) {
				t.Fatal("missing client trust root")
			}
			dialer := tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{clientCert}}}
			slow, dialErr := dialer.DialContext(t.Context(), "tcp", listener.Addr().String())
			if dialErr != nil {
				t.Fatal(dialErr)
			}
			t.Cleanup(func() { _ = slow.Close() })
			if err = slow.SetDeadline(time.Now().Add(2500 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if _, err = io.WriteString(slow, "POST /v1/qualification HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"); err != nil {
				t.Fatal(err)
			}
			_, readErr := io.ReadAll(slow)
			var timeout net.Error
			if errors.As(readErr, &timeout) && timeout.Timeout() {
				t.Fatal("slow body exceeded authority timeout", readErr)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatal("slow body held reader beyond budget", elapsed)
			}
			_ = slow.Close()
		}
		if _, err = client.Verify(t.Context(), f.request.Deployment); err != nil {
			t.Fatal("installed mTLS verification", err)
		}
		if attempt == 0 {
			revoked := f.policy
			revoked.Grants = nil
			writeInstalledPolicy(t, f.cfg.Policy, revoked)
		} else {
			serverCfg.Authority.Source = "changed"
			writeInstalledPolicy(t, name, serverCfg)
		}
		if _, err = client.Verify(t.Context(), f.request.Deployment); err == nil {
			t.Fatal("changed authority accepted")
		}
		client.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("authority shutdown did not join")
		}
		writeInstalledPolicy(t, f.cfg.Policy, f.policy)
	}
}

func datedProbe(output string, at time.Time) []byte {
	return []byte(strings.ReplaceAll(output, `{"Action":`, `{"Time":"`+at.UTC().Format(time.RFC3339Nano)+`","Action":`))
}

func TestQualificationAuthorityExpiryIncludesOldestProbe(t *testing.T) {
	f := newAuthorityFixture(t)
	oldest := time.Now().Add(-time.Hour + 10*time.Second).UTC()
	f.record.Probes[0].Output = credentialPin(t, f.record.Probes[0].Output.Path, datedProbe(passingProbe, oldest))
	f.save(t)
	got, err := f.authority.evaluate(t.Context(), f.request, f.client)
	if err != nil || got.ValidUntil.After(oldest.Add(time.Hour)) {
		t.Fatal("old probe validity extended", err)
	}
}

func TestQualificationProbeLifecycle(t *testing.T) {
	cases := []struct {
		name, events string
		want         bool
	}{
		{"unknown-output", "run:root output:root/sub pass:root pass:", false},
		{"unknown-pause", "run:root pause:root/sub pass:root pass:", false},
		{"unknown-cont", "run:root cont:root/sub pass:root pass:", false},
		{"early-parent-pass", "run:root run:root/sub pass:root pass:root/sub pass:", false},
		{"empty-pause", "run:root pause: pass:root pass:", false},
		{"cont-without-pause", "run:root cont:root pass:root pass:", false},
		{"double-pause", "run:root pause:root pause:root cont:root pass:root pass:", false},
		{"paused-pass", "run:root pause:root pass:root pass:", false},
		{"late-output", "run:root pass:root output:root pass:", false},
		{"late-descendant", "run:root run:root/sub pass:root/sub run:root/sub/nested pass:root/sub/nested pass:root pass:", false},
		{"parallel-slash-name", "run:root run:root/a/b pause:root/a/b output:root cont:root/a/b output:root/a/b pass:root/a/b pass:root output: pass:", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := []map[string]string{{"Action": "start", "Package": "live/probes"}}
			for _, token := range strings.Fields(tc.events) {
				action, name, _ := strings.Cut(token, ":")
				events = append(events, map[string]string{"Action": action, "Package": "live/probes", "Test": strings.Replace(name, "root", "TestLiveProbe", 1)})
			}
			var body bytes.Buffer
			for _, event := range events {
				event["Time"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
				if err := json.NewEncoder(&body).Encode(event); err != nil {
					t.Fatal(err)
				}
			}
			_, passed := passedQualificationProbe(body.Bytes(), QualificationProbe{Package: "live/probes", Test: "TestLiveProbe"}, time.Now(), time.Hour)
			if passed != tc.want {
				t.Fatalf("passed=%v want=%v", passed, tc.want)
			}
		})
	}
}
