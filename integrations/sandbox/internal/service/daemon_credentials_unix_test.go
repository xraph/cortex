//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

type daemonCredentialFixture struct {
	files                        DaemonCredentialFiles
	public                       ed25519.PublicKey
	signing                      ed25519.PrivateKey
	certificate, tlsKey, ca      []byte
	clientCertificate, clientKey []byte
}

func credentialPEM(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func credentialPin(t *testing.T, name string, body []byte) hostfs.Pin {
	t.Helper()
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return hostfs.Pin{Path: name, SHA256: evidence.Digest(body), Bytes: int64(len(body)), Mode: 0o600}
}

func daemonCredentialsFixture(t *testing.T) daemonCredentialFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caPublic, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test authority"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, template, template, caPublic, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverPublic, serverKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: template.NotBefore, NotAfter: template.NotAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	serverDER, err := x509.CreateCertificate(rand.Reader, leaf, template, serverPublic, caKey)
	if err != nil {
		t.Fatal(err)
	}
	f := daemonCredentialFixture{public: public, signing: signing, certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), tlsKey: credentialPEM(t, serverKey), ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})}
	clientPublic, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client := &x509.Certificate{SerialNumber: big.NewInt(3), NotBefore: template.NotBefore, NotAfter: template.NotAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	clientDER, err := x509.CreateCertificate(rand.Reader, client, template, clientPublic, caKey)
	if err != nil {
		t.Fatal(err)
	}
	f.clientCertificate, f.clientKey = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), credentialPEM(t, clientKey)
	f.files = DaemonCredentialFiles{SigningKey: credentialPin(t, filepath.Join(root, "signing.pem"), credentialPEM(t, signing)), Certificate: credentialPin(t, filepath.Join(root, "server.pem"), f.certificate), TLSKey: credentialPin(t, filepath.Join(root, "tls-key.pem"), f.tlsKey), ClientCAs: credentialPin(t, filepath.Join(root, "clients.pem"), f.ca)}
	return f
}

func TestDaemonCredentialsServeSignedBrokerOverMTLS(t *testing.T) {
	instance, config, _, job, _ := runtimeBrokerFixture(t)
	f := daemonCredentialsFixture(t)
	f.files.SigningKey = credentialPin(t, f.files.SigningKey.Path, credentialPEM(t, config.SigningKey))
	var calls atomic.Int32
	var denied atomic.Bool
	loaded, err := readDaemonCredentials(t.Context(), f.files, instance.PublicKey(), func(request *http.Request) (broker.Identity, error) {
		calls.Add(1)
		if request.TLS == nil || len(request.TLS.VerifiedChains) == 0 || denied.Load() {
			return broker.Identity{}, broker.ErrDenied
		}
		return broker.Identity{Scope: job.Scope, Principal: job.Principal}, nil
	}, credentialLimits(), uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loaded.Server.Serve(ctx, listener, instance) }()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.ca) {
		t.Fatal("fixture CA missing")
	}
	certificate, err := tls.X509KeyPair(f.clientCertificate, f.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "localhost", Certificates: []tls.Certificate{certificate}}}
	defer transport.CloseIdleConnections()
	client, err := broker.NewClient("https://"+listener.Addr().String(), &http.Client{Transport: transport, Timeout: 3 * time.Second}, instance.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	// Installed file removal after loading must not change the server snapshot.
	if err = os.Remove(f.files.TLSKey.Path); err != nil {
		t.Fatal(err)
	}
	receipt, err := client.Submit(t.Context(), job)
	if err != nil {
		t.Fatal("installed mTLS broker submit", err)
	}
	if err = broker.VerifyReceipt(receipt, instance.PublicKey()); err != nil {
		t.Fatal(err)
	}
	anonymous := transport.Clone()
	anonymous.TLSClientConfig.Certificates = nil
	defer anonymous.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+listener.Addr().String()+"/v1/capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, requestErr := (&http.Client{Transport: anonymous, Timeout: time.Second}).Do(request)
	if response != nil {
		response.Body.Close()
	}
	if requestErr == nil || calls.Load() != 1 {
		t.Fatal("missing client certificate reached authentication", requestErr, calls.Load())
	}
	denied.Store(true)
	if _, err = client.Submit(t.Context(), job); err == nil || calls.Load() != 2 {
		t.Fatal("verified client bypassed current authentication", err, calls.Load())
	}
	cancel()
	if err = awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("installed server did not join", err)
	}
}

func credentialAuthentication(*http.Request) (broker.Identity, error) {
	return broker.Identity{}, broker.ErrDenied
}

func credentialLimits() broker.ServerLimits {
	return broker.ServerLimits{Connections: 4, HeaderBytes: 4096, HeaderTimeout: time.Second, RequestTimeout: time.Second, IdleTimeout: time.Second}
}

func TestDaemonCredentialsBindSigningIdentityAndServer(t *testing.T) {
	f := daemonCredentialsFixture(t)
	loaded, err := readDaemonCredentials(t.Context(), f.files, f.public, credentialAuthentication, credentialLimits(), uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Server == nil || !bytes.Equal(loaded.SigningKey, f.signing) {
		t.Fatal("credentials not assembled")
	}
	message := []byte("receipt identity fixture")
	if !ed25519.Verify(f.public, message, ed25519.Sign(loaded.SigningKey, message)) {
		t.Fatal("wrong receipt signing identity")
	}
}

func TestDaemonCredentialsRejectTrailingKeyDER(t *testing.T) {
	for _, kind := range []string{"signing", "TLS"} {
		t.Run(kind, func(t *testing.T) {
			f := daemonCredentialsFixture(t)
			target, body := &f.files.TLSKey, f.tlsKey
			if kind == "signing" {
				target, body = &f.files.SigningKey, credentialPEM(t, f.signing)
			}
			block, _ := pem.Decode(body)
			block.Bytes = append(block.Bytes, 0x05, 0x00)
			*target = credentialPin(t, target.Path, pem.EncodeToMemory(block))
			loaded, err := readDaemonCredentials(t.Context(), f.files, f.public, credentialAuthentication, credentialLimits(), uint32(os.Geteuid()))
			if err == nil || loaded != nil {
				t.Fatal("trailing DER accepted", err)
			}
		})
	}
}

func TestDaemonCredentialsRejectPartialAndUnsafeConfiguration(t *testing.T) {
	for _, kind := range []string{"identity", "missing identity", "pin", "mode", "symlink", "hardlink", "oversize", "wrong tls key", "signing trailing", "signing prefix", "signing malformed prefix", "signing type", "certificate trailing", "too many certificates", "CA trailing", "CA leaf", "CA duplicate", "missing CA", "missing authentication", "limits", "owner", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := daemonCredentialsFixture(t)
			auth := broker.Authenticator(credentialAuthentication)
			limits := credentialLimits()
			owner := uint32(os.Geteuid())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "identity":
				f.public = bytes.Repeat([]byte{3}, ed25519.PublicKeySize)
			case "missing identity":
				f.public = nil
			case "pin":
				f.files.SigningKey.SHA256 = evidence.Digest([]byte("changed"))
			case "mode":
				if err := os.Chmod(f.files.SigningKey.Path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(f.files.SigningKey.Path, f.files.SigningKey.Path+"-alias"); err != nil {
					t.Fatal(err)
				}
				f.files.SigningKey.Path += "-alias"
			case "hardlink":
				if err := os.Link(f.files.SigningKey.Path, f.files.SigningKey.Path+"-alias"); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				f.files.SigningKey.Bytes = 1 << 20
			case "wrong tls key":
				f.files.TLSKey = f.files.SigningKey
			case "signing trailing":
				f.files.SigningKey = credentialPin(t, f.files.SigningKey.Path, append(credentialPEM(t, f.signing), []byte("ignored text")...))
			case "signing prefix":
				f.files.SigningKey = credentialPin(t, f.files.SigningKey.Path, append([]byte("ignored text\n"), credentialPEM(t, f.signing)...))
			case "signing malformed prefix":
				f.files.SigningKey = credentialPin(t, f.files.SigningKey.Path, append([]byte("-----BEGIN PRIVATE KEY-----\ninvalid!\n"), credentialPEM(t, f.signing)...))
			case "signing type":
				f.files.SigningKey = credentialPin(t, f.files.SigningKey.Path, f.certificate)
			case "certificate trailing":
				f.files.Certificate = credentialPin(t, f.files.Certificate.Path, append(f.certificate, []byte("ignored text")...))
			case "too many certificates":
				f.files.Certificate = credentialPin(t, f.files.Certificate.Path, bytes.Repeat(f.certificate, 9))
			case "CA trailing":
				f.files.ClientCAs = credentialPin(t, f.files.ClientCAs.Path, append(f.ca, []byte("ignored text")...))
			case "CA leaf":
				f.files.ClientCAs = credentialPin(t, f.files.ClientCAs.Path, f.certificate)
			case "CA duplicate":
				f.files.ClientCAs = credentialPin(t, f.files.ClientCAs.Path, bytes.Repeat(f.ca, 2))
			case "missing CA":
				f.files.ClientCAs = hostfs.Pin{}
			case "missing authentication":
				auth = nil
			case "limits":
				limits.Connections = 0
			case "owner":
				owner++
			case "cancelled":
				cancel()
			}
			loaded, err := readDaemonCredentials(ctx, f.files, f.public, auth, limits, owner)
			if err == nil || loaded != nil {
				t.Fatal("unsafe credentials accepted", err)
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
