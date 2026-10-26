//go:build linux || darwin

package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

func qualificationClientFixture(t *testing.T, handler func(QualificationRequest, http.ResponseWriter)) (*QualificationClient, QualificationClientConfig, string) {
	t.Helper()
	f := daemonCredentialsFixture(t)
	serverCert, err := tls.X509KeyPair(f.certificate, f.tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(f.ca) {
		t.Fatal("CA parse")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/qualification" || r.Header.Get("Content-Type") != "application/json" || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			t.Error("missing authenticated request")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 65537))
		var request QualificationRequest
		if readErr != nil || decodeCanonical(body, &request) != nil || request.SchemaVersion != 1 || len(request.Nonce) != 64 {
			t.Error("invalid request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		handler(request, w)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca}
	server.StartTLS()
	t.Cleanup(server.Close)
	leaf, err := x509.ParseCertificate(serverCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(f.files.SigningKey.Path)
	cfg := QualificationClientConfig{SchemaVersion: 1, Endpoint: strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/v1/qualification", Source: "qualification:test-authority", ServerSPKI: evidence.Digest(leaf.RawSubjectPublicKeyInfo), Certificate: credentialPin(t, filepath.Join(root, "qual-client.pem"), f.clientCertificate), TLSKey: credentialPin(t, filepath.Join(root, "qual-key.pem"), f.clientKey), ServerCAs: f.files.ClientCAs, Timeout: time.Second, MaxEvidenceAge: time.Hour, Readers: 2}
	name := filepath.Join(root, "qualification.json")
	writeInstalledPolicy(t, name, cfg)
	client, err := readQualificationClient(t.Context(), name, uint32(os.Geteuid()), func(context.Context) (QualificationHost, error) { return qualificationHostFixture(), nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, cfg, name
}

func qualificationHostFixture() QualificationHost {
	return QualificationHost{MachineID: evidence.Digest([]byte("host")), BootID: "12345678-1234-1234-1234-123456789012", KernelRelease: "6.1.1", KernelVersion: "test-kernel", Architecture: "x86_64"}
}
func remoteQualificationFixture(request QualificationRequest) QualificationResponse {
	now := time.Now().UTC()
	response := QualificationResponse{SchemaVersion: 1, Source: request.Source, Nonce: request.Nonce, Host: request.Host, Status: "qualified", CheckedAt: now, ValidUntil: now.Add(time.Minute), Qualification: qualificationFixture(request.Deployment), Evidence: QualificationEvidence{Kind: "live-systemd-runsc-v1", Reference: "evidence:live-probe-123", SHA256: evidence.Digest([]byte("retained probe log")), ObservedAt: now.Add(-time.Minute), ControlsDigest: request.Deployment.ProfileDigest, Checks: qualificationChecks()}}
	response.Qualification.Capabilities.Qualification = "live-systemd-runsc:123"
	response.Qualification.Capabilities.QualifiedUntil = response.ValidUntil
	return response
}
func writeRemoteQualification(t *testing.T, w http.ResponseWriter, response QualificationResponse) {
	t.Helper()
	body, err := json.Marshal(response)
	if err != nil {
		t.Error(err)
		return
	}
	if _, err = w.Write(body); err != nil {
		t.Error(err)
	}
}
func qualificationDeploymentFixture() DeploymentIdentity {
	return DeploymentIdentity{Broker: "broker", ProfileDigest: evidence.Digest([]byte("profile")), RuntimeDigest: evidence.Digest([]byte("runtime"))}
}

func TestQualificationClientAuthenticatesAndRechecksRevocation(t *testing.T) {
	var calls atomic.Int32
	client, _, _ := qualificationClientFixture(t, func(request QualificationRequest, w http.ResponseWriter) {
		response := remoteQualificationFixture(request)
		if calls.Add(1) > 1 {
			response.Status = "revoked"
		}
		writeRemoteQualification(t, w, response)
	})
	identity := qualificationDeploymentFixture()
	if got, err := client.Verify(t.Context(), identity); err != nil || got.Deployment != identity {
		t.Fatal("authenticated authority refused", err)
	}
	if got, err := client.Verify(t.Context(), identity); err == nil || got != (DeploymentQualification{}) {
		t.Fatal("revoked response reused", err)
	}
	if calls.Load() != 2 {
		t.Fatal("qualification was cached")
	}
}

func TestQualificationClientRejectsUnboundAndIncompleteEvidence(t *testing.T) {
	for _, mode := range []string{"nonce", "source", "host", "boot", "kernel", "deployment", "runtime", "fixture", "fixture-id", "capability-expiry", "capability-extension", "capability-limits", "partial", "duplicate-check", "controls", "missing-reference", "bad-digest", "stale", "future", "stale-revocation", "expired", "extended", "schema", "unknown", "trailing", "oversized", "content-type", "http-error", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			client, _, _ := qualificationClientFixture(t, func(request QualificationRequest, w http.ResponseWriter) {
				response := remoteQualificationFixture(request)
				switch mode {
				case "nonce":
					response.Nonce = strings.Repeat("a", 64)
				case "source":
					response.Source = "other"
				case "host":
					response.Host.MachineID = evidence.Digest([]byte("other"))
				case "boot":
					response.Host.BootID = "other"
				case "kernel":
					response.Host.KernelRelease = "other"
				case "deployment":
					response.Qualification.Deployment.ProfileDigest = evidence.Digest([]byte("other"))
				case "runtime":
					response.Qualification.Capabilities.RuntimeDigest = evidence.Digest([]byte("other"))
				case "fixture":
					response.Evidence.Kind = "fixture"
				case "fixture-id":
					response.Qualification.Capabilities.Qualification = "fixture-only"
				case "capability-expiry":
					response.Qualification.Capabilities.QualifiedUntil = time.Now().Add(-time.Minute)
				case "capability-extension":
					response.Qualification.Capabilities.QualifiedUntil = response.ValidUntil.Add(time.Minute)
				case "capability-limits":
					response.Qualification.Capabilities.Limits.Processes = 0
				case "partial":
					response.Evidence.Checks = response.Evidence.Checks[1:]
				case "duplicate-check":
					response.Evidence.Checks[1] = response.Evidence.Checks[0]
				case "controls":
					response.Evidence.ControlsDigest = evidence.Digest([]byte("other"))
				case "missing-reference":
					response.Evidence.Reference = ""
				case "bad-digest":
					response.Evidence.SHA256 = "invalid"
				case "stale":
					response.Evidence.ObservedAt = time.Now().Add(-2 * time.Hour)
				case "future":
					response.Evidence.ObservedAt = time.Now().Add(time.Hour)
				case "stale-revocation":
					response.CheckedAt = time.Now().Add(-time.Minute)
				case "expired":
					response.ValidUntil = time.Now().Add(-time.Minute)
				case "extended":
					response.ValidUntil = time.Now().Add(2 * time.Hour)
				case "schema":
					response.SchemaVersion++
				case "unknown":
					_, _ = w.Write([]byte(`{"schema_version":1,"unknown":true}`))
					return
				case "trailing":
					_, _ = w.Write([]byte(`{} {}`))
					return
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat("x", 65537)))
					return
				case "content-type":
					w.Header().Set("Content-Type", "text/plain")
				case "http-error":
					w.WriteHeader(http.StatusForbidden)
				case "redirect":
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				writeRemoteQualification(t, w, response)
			})
			if got, err := client.Verify(t.Context(), qualificationDeploymentFixture()); err == nil || got != (DeploymentQualification{}) {
				t.Fatal("unsafe authority response accepted")
			}
		})
	}
}

func TestQualificationClientRechecksProtectedTrust(t *testing.T) {
	for _, mode := range []string{"config", "credentials", "pin", "wrong-ca"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			client, cfg, name := qualificationClientFixture(t, func(request QualificationRequest, w http.ResponseWriter) {
				calls.Add(1)
				writeRemoteQualification(t, w, remoteQualificationFixture(request))
			})
			switch mode {
			case "config":
				cfg.Source = "changed"
				writeInstalledPolicy(t, name, cfg)
			case "credentials":
				if err := os.Chmod(cfg.TLSKey.Path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "pin":
				cfg.ServerSPKI = evidence.Digest([]byte("other"))
				writeInstalledPolicy(t, name, cfg)
			case "wrong-ca":
				cfg.ServerCAs = hostfs.Pin{}
				writeInstalledPolicy(t, name, cfg)
			}
			if _, err := client.Verify(t.Context(), qualificationDeploymentFixture()); err == nil || calls.Load() != 0 {
				t.Fatal("changed trust reached authority", err)
			}
		})
	}
}

func TestQualificationClientRejectsWrongTLSPin(t *testing.T) {
	var calls atomic.Int32
	_, cfg, name := qualificationClientFixture(t, func(request QualificationRequest, w http.ResponseWriter) {
		calls.Add(1)
		writeRemoteQualification(t, w, remoteQualificationFixture(request))
	})
	cfg.ServerSPKI = evidence.Digest([]byte("untrusted server"))
	writeInstalledPolicy(t, name, cfg)
	client, err := readQualificationClient(t.Context(), name, uint32(os.Geteuid()), func(context.Context) (QualificationHost, error) { return qualificationHostFixture(), nil })
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Verify(t.Context(), qualificationDeploymentFixture()); err == nil || calls.Load() != 0 {
		t.Fatal("wrong server key reached authority", err)
	}
}

func TestQualificationClientIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	client, _, _ := qualificationClientFixture(t, func(request QualificationRequest, w http.ResponseWriter) {
		writeRemoteQualification(t, w, remoteQualificationFixture(request))
	})
	if _, err := client.Verify(t.Context(), qualificationDeploymentFixture()); err != nil {
		t.Fatal("environment redirected authority", err)
	}
}

func TestQualificationClientRejectsChangedHostDuringRequest(t *testing.T) {
	client, _, _ := qualificationClientFixture(t, func(request QualificationRequest, w http.ResponseWriter) {
		writeRemoteQualification(t, w, remoteQualificationFixture(request))
	})
	observations := 0
	client.observe = func(context.Context) (QualificationHost, error) {
		observations++
		host := qualificationHostFixture()
		if observations > 1 {
			host.KernelVersion = "changed"
		}
		return host, nil
	}
	if _, err := client.Verify(t.Context(), qualificationDeploymentFixture()); err == nil || observations != 2 {
		t.Fatal("host changed during qualification", err)
	}
}

func TestQualificationClientBoundsCancellationAndConcurrentRequests(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			client, _, _ := qualificationClientFixture(t, func(_ QualificationRequest, _ http.ResponseWriter) { close(entered); <-release })
			defer close(release)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "capacity" {
				client.slots <- struct{}{}
				client.slots <- struct{}{}
				if _, err := client.Verify(ctx, qualificationDeploymentFixture()); err == nil {
					t.Fatal("capacity overflow admitted")
				}
				<-client.slots
				<-client.slots
				return
			}
			done := make(chan error, 1)
			go func() { _, err := client.Verify(ctx, qualificationDeploymentFixture()); done <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("authority never received request")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unfinished qualification accepted")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("qualification ignored deadline")
			}
		})
	}
}

func TestQualificationClientRejectsUnsafeConfig(t *testing.T) {
	for _, mode := range []string{"schema", "http", "userinfo", "query", "path", "source", "pin", "timeout", "age", "readers", "alias", "mode", "relative", "unknown", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			_, cfg, name := qualificationClientFixture(t, func(_ QualificationRequest, _ http.ResponseWriter) { t.Error("construction contacted authority") })
			switch mode {
			case "schema":
				cfg.SchemaVersion++
			case "http":
				cfg.Endpoint = strings.Replace(cfg.Endpoint, "https:", "http:", 1)
			case "userinfo":
				cfg.Endpoint = strings.Replace(cfg.Endpoint, "https://", "https://user@", 1)
			case "query":
				cfg.Endpoint += "?qualified=true"
			case "path":
				cfg.Endpoint += "/other"
			case "source":
				cfg.Source = ""
			case "pin":
				cfg.ServerSPKI = ""
			case "timeout":
				cfg.Timeout = 11 * time.Second
			case "age":
				cfg.MaxEvidenceAge = 25 * time.Hour
			case "readers":
				cfg.Readers = 33
			case "alias":
				cfg.TLSKey.Path = cfg.Certificate.Path
			case "mode":
				cfg.TLSKey.Mode = 0o644
			case "relative":
				cfg.Certificate.Path = "client.pem"
			}
			writeInstalledPolicy(t, name, cfg)
			if mode == "unknown" || mode == "duplicate" {
				body, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				field := `"unknown":true,`
				if mode == "duplicate" {
					field = `"schema_version":1,`
				}
				body = append([]byte("{"+field), body[1:]...)
				if err = os.WriteFile(name, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			client, err := readQualificationClient(t.Context(), name, uint32(os.Geteuid()), func(context.Context) (QualificationHost, error) { return qualificationHostFixture(), nil })
			if client != nil {
				client.Close()
			}
			if err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestQualificationClientLimitsActualConcurrentObservations(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	client, _, _ := qualificationClientFixture(t, func(_ QualificationRequest, _ http.ResponseWriter) {
		entered <- struct{}{}
		<-release
	})
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := client.Verify(ctx, qualificationDeploymentFixture()); done <- err }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent request never reached authority")
		}
	}
	if _, err := client.Verify(ctx, qualificationDeploymentFixture()); err == nil {
		t.Fatal("third observation exceeded capacity")
	}
	cancel()
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled observation accepted")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("active observations failed to join")
		}
	}
}

func TestQualificationClientRequiresCAHostnameAndClientIdentity(t *testing.T) {
	for _, mode := range []string{"ca", "hostname", "client"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			_, cfg, name := qualificationClientFixture(t, func(request QualificationRequest, w http.ResponseWriter) {
				calls.Add(1)
				writeRemoteQualification(t, w, remoteQualificationFixture(request))
			})
			foreign := daemonCredentialsFixture(t)
			switch mode {
			case "ca":
				cfg.ServerCAs = foreign.files.ClientCAs
			case "hostname":
				cfg.Endpoint = strings.Replace(cfg.Endpoint, "localhost", "127.0.0.1", 1)
			case "client":
				cfg.Certificate = credentialPin(t, filepath.Join(filepath.Dir(foreign.files.Certificate.Path), "foreign-client.pem"), foreign.clientCertificate)
				cfg.TLSKey = credentialPin(t, filepath.Join(filepath.Dir(foreign.files.TLSKey.Path), "foreign-key.pem"), foreign.clientKey)
			}
			writeInstalledPolicy(t, name, cfg)
			client, err := readQualificationClient(t.Context(), name, uint32(os.Geteuid()), func(context.Context) (QualificationHost, error) { return qualificationHostFixture(), nil })
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err = client.Verify(t.Context(), qualificationDeploymentFixture()); err == nil || calls.Load() != 0 {
				t.Fatal("unauthenticated identity reached authority", err)
			}
		})
	}
}
