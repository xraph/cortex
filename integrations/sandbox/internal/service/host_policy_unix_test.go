//go:build linux || darwin

package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func policyFixture(t *testing.T, provided ...daemonCredentialFixture) (*HostPolicy, HostPolicyDocument, sandbox.Job, *http.Request) {
	t.Helper()
	var f daemonCredentialFixture
	if len(provided) == 0 {
		f = daemonCredentialsFixture(t)
	} else {
		f = provided[0]
	}
	block, _ := pem.Decode(f.clientCertificate)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(f.ca)
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "policy-fixture", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "one"}}}, Principal: "operator", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/bin/true"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Second, OutputBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	document := HostPolicyDocument{SchemaVersion: 1, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), Subjects: []HostSubjectGrant{{Principal: job.Principal, Scope: job.Scope, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), Certificates: []string{evidence.Digest(leaf.Raw)}, Images: []string{job.Image}, Limits: job.Limits}}}
	name := filepath.Join(filepath.Dir(f.files.SigningKey.Path), "policy.json")
	writeInstalledPolicy(t, name, document)
	policy, err := newHostPolicy(t.Context(), name, uint32(os.Geteuid()), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost/v1/capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf, ca}}}
	return policy, document, job, request
}

func writeInstalledPolicy(t *testing.T, name string, document any) {
	t.Helper()
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name+".tmp", body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(name+".tmp", name); err != nil {
		t.Fatal(err)
	}
}

func TestHostPolicyRevocationThroughMTLSAndRecovery(t *testing.T) {
	original, config, controller, job, _ := runtimeBrokerFixture(t)
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	f := daemonCredentialsFixture(t)
	p, doc, _, _ := policyFixture(t, f)
	doc.Subjects[0].Principal, doc.Subjects[0].Scope = job.Principal, job.Scope
	doc.Subjects[0].Images, doc.Subjects[0].Limits = []string{job.Image}, job.Limits
	writeInstalledPolicy(t, p.path, doc)
	f.files.SigningKey = credentialPin(t, f.files.SigningKey.Path, credentialPEM(t, config.SigningKey))
	loaded, err := readDaemonCredentials(t.Context(), f.files, original.PublicKey(), p.Authenticate, credentialLimits(), uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	config.Authorize, config.SigningKey = p.Authorize, loaded.SigningKey
	controller.launcher.authorize, controller.scanner.revalidate = p.Revalidate, p.Revalidate
	instance, err := broker.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loaded.Server.Serve(ctx, listener, instance) }()
	transport := credentialTransport(t, f)
	defer transport.CloseIdleConnections()
	client, err := broker.NewClient("https://"+listener.Addr().String(), &http.Client{Transport: transport, Timeout: 3 * time.Second}, instance.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := client.Submit(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if err = broker.VerifyReceipt(receipt, instance.PublicKey()); err != nil {
		t.Fatal(err)
	}
	// Keep transport identity valid but revoke this job's execution capacity.
	doc.Subjects[0].Limits.MemoryBytes--
	writeInstalledPolicy(t, p.path, doc)
	if _, err = client.Submit(t.Context(), job); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("changed grant allowed a retry", err)
	}
	page, err := controller.scanner.Sweep(t.Context(), "")
	if err != nil || page.Cancelled != 1 || page.Reaped != 1 {
		t.Fatal("revoked job not fenced and reaped", page, err)
	}
	record, err := controller.launcher.store.Load(t.Context(), job.ID)
	if err != nil || record.Phase != workerstore.Terminal || record.Result == nil || record.Result.State != sandbox.StateCancelled {
		t.Fatal("revoked work survived recovery", err)
	}
	cancel()
	if err = awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHostPolicyCurrentGrantsAndCertificateRevocation(t *testing.T) {
	p, document, job, request := policyFixture(t)
	who, err := p.Authenticate(request)
	if err != nil || who.Principal != job.Principal || who.Scope.Canonical() != job.Scope.Canonical() {
		t.Fatal("identity mapping", err)
	}
	if err = p.Authorize(t.Context(), who, job); err != nil {
		t.Fatal(err)
	}
	if err = p.AuthorizeImage(t.Context(), job, job.Image); err != nil {
		t.Fatal(err)
	}
	document.Subjects[0].Certificates = nil
	writeInstalledPolicy(t, p.path, document)
	if _, err = p.Authenticate(request); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("revoked certificate accepted", err)
	}
	if err = p.Revalidate(t.Context(), job); err != nil {
		t.Fatal("transport revocation removed separate execution grant", err)
	}
	document.Subjects = nil
	writeInstalledPolicy(t, p.path, document)
	if err = p.Authorize(t.Context(), who, job); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("removed grant accepted", err)
	}
	if err = p.Revalidate(t.Context(), job); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("recovery retained removed grant", err)
	}
}

func TestHostPolicyGrantWindowAndDetachedIdentity(t *testing.T) {
	p, doc, job, r := policyFixture(t)
	start := p.now()
	doc.Subjects[0].NotBefore = start.Add(time.Second)
	doc.Subjects[0].NotAfter = start.Add(2 * time.Second)
	writeInstalledPolicy(t, p.path, doc)
	if _, err := p.Authenticate(r); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("future grant accepted", err)
	}
	p.now = func() time.Time { return start.Add(time.Second) }
	who, err := p.Authenticate(r)
	if err != nil {
		t.Fatal("grant start rejected", err)
	}
	who.Scope.Levels[0].Value = "mutated"
	again, err := p.Authenticate(r)
	if err != nil || again.Scope.Canonical() != job.Scope.Canonical() {
		t.Fatal("identity alias changed policy", err)
	}
	if err = p.Authorize(t.Context(), who, job); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("foreign identity accepted", err)
	}
	if err = p.AuthorizeImage(t.Context(), job, "sha256:"+strings.Repeat("b", 64)); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("unrelated image accepted", err)
	}
	p.now = func() time.Time { return start.Add(2 * time.Second) }
	if _, err = p.Authenticate(r); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("expired grant authenticated", err)
	}
	if err = p.Revalidate(t.Context(), job); !errors.Is(err, broker.ErrDenied) {
		t.Fatal("expired grant executed", err)
	}
}

func TestHostPolicyRejectsAmbientIdentityAndStaleTLS(t *testing.T) {
	for _, kind := range []string{"nil request", "plaintext", "unverified", "incomplete", "old TLS", "different leaf", "expired leaf", "expired CA", "future leaf", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			p, _, _, r := policyFixture(t)
			r.Header.Set("X-Principal", "operator")
			switch kind {
			case "nil request":
				r = nil
			case "plaintext":
				r.TLS = nil
			case "unverified":
				r.TLS.VerifiedChains = nil
			case "incomplete":
				r.TLS.HandshakeComplete = false
			case "old TLS":
				r.TLS.Version = tls.VersionTLS12
			case "different leaf":
				r.TLS.PeerCertificates = r.TLS.VerifiedChains[0][1:]
			case "expired leaf":
				p.now = func() time.Time { return r.TLS.PeerCertificates[0].NotAfter }
			case "expired CA":
				r.TLS.VerifiedChains[0][1].NotAfter = p.now()
			case "future leaf":
				r.TLS.PeerCertificates[0].NotBefore = p.now().Add(time.Hour)
			case "cancelled":
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			if _, err := p.Authenticate(r); err == nil {
				t.Fatal("unsafe TLS identity accepted")
			}
		})
	}
}

func TestHostPolicyFailsClosedOnInvalidOrUnavailableDocument(t *testing.T) {
	for _, kind := range []string{"missing", "permissions", "symlink", "trailing", "unknown", "expired", "future", "schema", "duplicate subject", "duplicate certificate", "duplicate image", "invalid image", "scope", "principal", "limits", "grant range", "too many subjects"} {
		t.Run(kind, func(t *testing.T) {
			p, doc, job, r := policyFixture(t)
			switch kind {
			case "schema":
				doc.SchemaVersion++
			case "expired":
				doc.NotAfter = p.now()
			case "future":
				doc.NotBefore = p.now().Add(time.Minute)
			case "duplicate subject":
				doc.Subjects = append(doc.Subjects, doc.Subjects[0])
			case "duplicate certificate":
				doc.Subjects[0].Certificates = append(doc.Subjects[0].Certificates, doc.Subjects[0].Certificates[0])
			case "duplicate image":
				doc.Subjects[0].Images = append(doc.Subjects[0].Images, doc.Subjects[0].Images[0])
			case "invalid image":
				doc.Subjects[0].Images = []string{"image:latest"}
			case "scope":
				doc.Subjects[0].Scope = cortex.Scope{}
			case "principal":
				doc.Subjects[0].Principal = ""
			case "limits":
				doc.Subjects[0].Limits.Processes = 0
			case "grant range":
				doc.Subjects[0].NotAfter = doc.NotAfter.Add(time.Hour)
			case "too many subjects":
				doc.Subjects = make([]HostSubjectGrant, 513)
			}
			writeInstalledPolicy(t, p.path, doc)
			switch kind {
			case "missing":
				if err := os.Remove(p.path); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(p.path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(p.path, p.path+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(p.path+"-real", p.path); err != nil {
					t.Fatal(err)
				}
			case "trailing", "unknown":
				body, err := os.ReadFile(p.path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "trailing" {
					body = append(body, '\n')
				} else {
					body = append([]byte(`{"unknown":true,`), body[1:]...)
				}
				if err = os.WriteFile(p.path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Revalidate(t.Context(), job); !errors.Is(err, broker.ErrDenied) {
				t.Fatal("invalid policy retained execution", err)
			}
			if _, err := p.Authenticate(r); !errors.Is(err, broker.ErrDenied) {
				t.Fatal("invalid policy retained authentication", err)
			}
		})
	}
}

func TestHostPolicyJobBindingAndLimits(t *testing.T) {
	for _, kind := range []string{"principal", "scope", "image", "cpu", "memory", "processes", "scratch", "wall", "output", "artifacts", "digest"} {
		t.Run(kind, func(t *testing.T) {
			p, _, job, r := policyFixture(t)
			who, err := p.Authenticate(r)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "principal":
				job.Principal = "other"
			case "scope":
				job.Scope.Levels = append(job.Scope.Levels, cortex.Level{Key: "project", Value: "child"})
			case "image":
				job.Image = "sha256:" + strings.Repeat("b", 64)
			case "cpu":
				job.Limits.CPUMillis++
			case "memory":
				job.Limits.MemoryBytes++
			case "processes":
				job.Limits.Processes++
			case "scratch":
				job.Limits.ScratchBytes++
			case "wall":
				job.Limits.WallTime++
			case "output":
				job.Limits.OutputBytes++
			case "artifacts":
				job.Limits.ArtifactBytes++
			}
			if kind != "digest" {
				job, err = sandbox.Seal(job)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				job.Argv[0] = "changed"
			}
			if err = p.Authorize(t.Context(), who, job); !errors.Is(err, broker.ErrDenied) {
				t.Fatal("job escaped grant", err)
			}
			if err = p.Revalidate(t.Context(), job); !errors.Is(err, broker.ErrDenied) {
				t.Fatal("recovery escaped grant", err)
			}
		})
	}
}
