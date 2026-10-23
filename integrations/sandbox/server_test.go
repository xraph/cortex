package sandboxbroker_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
)

func serverFixtureConfig(t *testing.T) (broker.ServerConfig, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return serverFixtureWithKey(t, key)
}

func serverFixtureWithKey(t *testing.T, key crypto.Signer) (broker.ServerConfig, *tls.Config) {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	cfg := broker.ServerConfig{Certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, Authenticate: func(*http.Request) (broker.Identity, error) { return identity(), nil }, Limits: broker.ServerLimits{Connections: 4, HeaderBytes: 4096, HeaderTimeout: time.Second, RequestTimeout: 5 * time.Second, IdleTimeout: time.Second}}
	return cfg, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
}

func startBrokerServer(t *testing.T, cfg broker.ServerConfig, clientTLS *tls.Config, instance *broker.Broker, provided ...net.Listener) (string, *http.Client, func() error) {
	t.Helper()
	server, err := broker.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var listener net.Listener
	if len(provided) == 1 {
		listener = provided[0]
	} else {
		listener, err = (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener, instance) }()
	var once sync.Once
	var result error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("TLS server did not join shutdown")
			}
		})
		return result
	}
	transport := &http.Transport{TLSClientConfig: clientTLS, TLSHandshakeTimeout: 2 * time.Second}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	t.Cleanup(func() { transport.CloseIdleConnections(); _ = stop() })
	return "https://" + listener.Addr().String(), client, stop
}

func TestTLSServerAuthenticatedBrokerRoundTrip(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	url, transport, stop := startBrokerServer(t, cfg, clientTLS, instance)
	client, err := broker.NewClient(url, transport, instance.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	task := job(t)
	if _, err = client.Submit(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err = client.Cancel(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Collect(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if err = stop(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestTLSServerCancelsAndJoinsActiveAuthentication(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	entered := make(chan struct{})
	var joined atomic.Bool
	cfg.Authenticate = func(request *http.Request) (broker.Identity, error) {
		close(entered)
		<-request.Context().Done()
		joined.Store(true)
		return broker.Identity{}, request.Context().Err()
	}
	url, client, stop := startBrokerServer(t, cfg, clientTLS, instance)
	response := make(chan error, 1)
	go func() {
		result, err := serverGET(t.Context(), client, url+"/v1/capabilities")
		if result != nil {
			_ = result.Body.Close()
		}
		response <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("authentication not called")
	}
	if err := stop(); !errors.Is(err, context.Canceled) || !joined.Load() {
		t.Fatal("active callback not joined", err)
	}
	select {
	case err := <-response:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client remained blocked")
	}
}

func TestTLSServerRequestDeadlineClosesOnlyItsConnection(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	cfg.Limits.HeaderTimeout = 100 * time.Millisecond
	cfg.Limits.RequestTimeout = 200 * time.Millisecond
	var calls atomic.Int32
	cancelled := make(chan struct{})
	cfg.Authenticate = func(request *http.Request) (broker.Identity, error) {
		if calls.Add(1) == 1 {
			<-request.Context().Done()
			close(cancelled)
			return broker.Identity{}, request.Context().Err()
		}
		return identity(), nil
	}
	url, client, _ := startBrokerServer(t, cfg, clientTLS, instance)
	result, err := serverGET(t.Context(), client, url+"/v1/capabilities")
	if result != nil {
		_ = result.Body.Close()
	}
	if err == nil {
		t.Fatal("expired request returned success")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("deadline did not cancel callback")
	}
	result, err = serverGET(t.Context(), client, url+"/v1/capabilities")
	if err != nil {
		t.Fatal("deadline stopped unrelated serving", err)
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		t.Fatal(result.StatusCode)
	}
}

func TestTLSServerRequiresConfiguredClientCertificate(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	cfg.ClientCAs = clientTLS.RootCAs
	var calls atomic.Int32
	cfg.Authenticate = func(request *http.Request) (broker.Identity, error) {
		calls.Add(1)
		if request.TLS == nil || len(request.TLS.VerifiedChains) == 0 {
			return broker.Identity{}, broker.ErrDenied
		}
		return identity(), nil
	}
	url, anonymous, _ := startBrokerServer(t, cfg, clientTLS, instance)
	response, err := serverGET(t.Context(), anonymous, url+"/v1/capabilities")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || calls.Load() != 0 {
		t.Fatal("unverified TLS client reached authorization", err)
	}
	authenticatedTLS := clientTLS.Clone()
	authenticatedTLS.Certificates = []tls.Certificate{cfg.Certificate}
	transport := &http.Transport{TLSClientConfig: authenticatedTLS}
	defer transport.CloseIdleConnections()
	authenticated := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err = serverGET(t.Context(), authenticated, url+"/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatal("verified client rejected", response.StatusCode)
	}
}

func TestTLSServerRejectsMissingOrUnboundedConfiguration(t *testing.T) {
	for _, change := range []string{"certificate", "key", "mismatched-key", "auth", "connections", "headers", "header-time", "request-time", "idle-time", "nil-ecdsa", "nil-rsa", "short-ed25519", "empty-ecdsa", "empty-rsa", "nil-scalar", "intermediate"} {
		t.Run(change, func(t *testing.T) {
			cfg, _ := serverFixtureConfig(t)
			switch change {
			case "certificate":
				cfg.Certificate.Certificate = nil
			case "key":
				cfg.Certificate.PrivateKey = nil
			case "mismatched-key":
				other, _ := serverFixtureConfig(t)
				cfg.Certificate.PrivateKey = other.Certificate.PrivateKey
			case "auth":
				cfg.Authenticate = nil
			case "connections":
				cfg.Limits.Connections = 0
			case "headers":
				cfg.Limits.HeaderBytes = 1 << 20
			case "header-time":
				cfg.Limits.HeaderTimeout = 0
			case "request-time":
				cfg.Limits.RequestTimeout = time.Hour
			case "idle-time":
				cfg.Limits.IdleTimeout = 0
			case "nil-ecdsa":
				cfg.Certificate.PrivateKey = (*ecdsa.PrivateKey)(nil)
			case "nil-rsa":
				cfg.Certificate.PrivateKey = (*rsa.PrivateKey)(nil)
			case "short-ed25519":
				cfg.Certificate.PrivateKey = ed25519.PrivateKey{1}
			case "empty-ecdsa":
				cfg.Certificate.PrivateKey = &ecdsa.PrivateKey{}
			case "empty-rsa":
				cfg.Certificate.PrivateKey = &rsa.PrivateKey{}
			case "nil-scalar":
				cfg.Certificate.PrivateKey = &ecdsa.PrivateKey{PublicKey: cfg.Certificate.PrivateKey.(*ecdsa.PrivateKey).PublicKey}
			case "intermediate":
				cfg.Certificate.Certificate = append(cfg.Certificate.Certificate, []byte("invalid DER"))
			}
			if _, err := broker.NewServer(cfg); err == nil {
				t.Fatal("unsafe server configuration accepted")
			}
		})
	}
}

func TestTLSServerDoesNotReleaseWorkAdmissionUntilHandlerReturns(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	cfg.Limits.Connections = 1
	cfg.Limits.HeaderTimeout = 100 * time.Millisecond
	cfg.Limits.RequestTimeout = 200 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	cfg.Authenticate = func(request *http.Request) (broker.Identity, error) {
		if calls.Add(1) == 1 {
			<-release
			return broker.Identity{}, request.Context().Err()
		}
		return identity(), nil
	}
	url, client, _ := startBrokerServer(t, cfg, clientTLS, instance)
	response, err := serverGET(t.Context(), client, url+"/v1/capabilities")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("expired request succeeded")
	}
	response, err = serverGET(t.Context(), client, url+"/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatal("unfinished handler released work admission", response.StatusCode, calls.Load())
	}
}

func TestTLSServerBoundsConnectionsBeforeAuthentication(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	cfg.Limits.Connections = 1
	cfg.Limits.HeaderTimeout = 5 * time.Second
	cfg.Limits.IdleTimeout = 5 * time.Second
	var calls atomic.Int32
	cfg.Authenticate = func(*http.Request) (broker.Identity, error) { calls.Add(1); return identity(), nil }
	url, client, _ := startBrokerServer(t, cfg, clientTLS, instance)
	address := strings.TrimPrefix(url, "https://")
	first, err := serverDialTLS(t.Context(), address, clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	deadline, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	second, err := (&tls.Dialer{Config: clientTLS}).DialContext(deadline, "tcp", address)
	if second != nil {
		_ = second.Close()
	}
	if err == nil || calls.Load() != 0 {
		t.Fatal("connection cap did not hold admission", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := serverGET(t.Context(), client, url+"/v1/capabilities")
	if err != nil {
		t.Fatal("connection capacity did not recover", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatal(response.StatusCode, calls.Load())
	}
}

func TestTLSServerFreezesCertificateAndPrivateKey(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	cfg.Certificate.Leaf = &x509.Certificate{NotAfter: time.Time{}}
	url, client, _ := startBrokerServer(t, cfg, clientTLS, instance)
	cfg.Certificate.Certificate[0][0] ^= 1
	*cfg.Certificate.PrivateKey.(*ecdsa.PrivateKey) = ecdsa.PrivateKey{}
	response, err := serverGET(t.Context(), client, url+"/v1/capabilities")
	if err != nil {
		t.Fatal("caller mutated retained certificate material", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
}

func TestTLSServerRejectsPlaintextOldTLSAndOversizedHeaders(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	var calls atomic.Int32
	cfg.Authenticate = func(*http.Request) (broker.Identity, error) { calls.Add(1); return identity(), nil }
	url, client, _ := startBrokerServer(t, cfg, clientTLS, instance)
	plaintext := &http.Client{Timeout: 3 * time.Second}
	response, err := serverGET(t.Context(), plaintext, strings.Replace(url, "https://", "http://", 1)+"/v1/capabilities")
	if response != nil {
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("plaintext admitted")
		}
	}
	if err == nil && response == nil {
		t.Fatal("missing plaintext observation")
	}
	oldTLS := clientTLS.Clone()
	oldTLS.MinVersion, oldTLS.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	connection, err := serverDialTLS(t.Context(), strings.TrimPrefix(url, "https://"), oldTLS)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil {
		t.Fatal("TLS 1.2 admitted")
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/v1/capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Oversized", strings.Repeat("x", 32<<10))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge || calls.Load() != 0 {
		t.Fatal("unsafe request reached authentication", response.StatusCode, calls.Load())
	}
}

func TestTLSServerStopsSlowRequestBodyWithoutDispatch(t *testing.T) {
	instance, runtime, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	cfg.Limits.HeaderTimeout = 100 * time.Millisecond
	cfg.Limits.RequestTimeout = 200 * time.Millisecond
	url, _, _ := startBrokerServer(t, cfg, clientTLS, instance)
	connection, err := serverDialTLS(t.Context(), strings.TrimPrefix(url, "https://"), clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err = connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(connection, "POST /v1/jobs HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(connection)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("slow request exceeded transport deadline", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 {
		t.Fatal("partial body dispatched a job")
	}
}

func TestTLSServerSupportsDetachedRSAAndEd25519Keys(t *testing.T) {
	for _, algorithm := range []string{"rsa", "ed25519"} {
		t.Run(algorithm, func(t *testing.T) {
			instance, _, _, _ := setup(t)
			var key crypto.Signer
			if algorithm == "rsa" {
				generated, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
				key = generated
			} else {
				_, generated, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				key = generated
			}
			cfg, clientTLS := serverFixtureWithKey(t, key)
			if rsaKey, ok := key.(*rsa.PrivateKey); ok {
				rsaKey.Precomputed = rsa.PrecomputedValues{}
			}
			url, client, _ := startBrokerServer(t, cfg, clientTLS, instance)
			if rsaKey, ok := key.(*rsa.PrivateKey); ok && rsaKey.Precomputed.Dp != nil {
				t.Fatal("server mutated caller RSA precomputation")
			}
			response, err := serverGET(t.Context(), client, url+"/v1/capabilities")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatal(response.StatusCode)
			}
		})
	}
}

func TestTLSServerFreezesClientTrustAndDisablesMultiplexing(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	cfg.ClientCAs = clientTLS.RootCAs
	url, _, _ := startBrokerServer(t, cfg, clientTLS, instance)
	other, _ := serverFixtureConfig(t)
	certificate, err := x509.ParseCertificate(other.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	cfg.ClientCAs.AddCert(certificate)
	foreignTLS := clientTLS.Clone()
	foreignTLS.Certificates = []tls.Certificate{other.Certificate}
	foreignTransport := &http.Transport{TLSClientConfig: foreignTLS}
	defer foreignTransport.CloseIdleConnections()
	foreign := &http.Client{Transport: foreignTransport, Timeout: 3 * time.Second}
	response, err := serverGET(t.Context(), foreign, url+"/v1/capabilities")
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("caller changed retained client trust")
	}
	permitted := clientTLS.Clone()
	permitted.Certificates = []tls.Certificate{cfg.Certificate}
	permitted.NextProtos = []string{"h2", "http/1.1"}
	connection, err := serverDialTLS(t.Context(), strings.TrimPrefix(url, "https://"), permitted)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if connection.ConnectionState().NegotiatedProtocol != "http/1.1" {
		t.Fatal("multiplexed protocol bypassed connection work bounds")
	}
}

func serverGET(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

func serverDialTLS(ctx context.Context, address string, config *tls.Config) (*tls.Conn, error) {
	connection, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 3 * time.Second}, Config: config}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	encrypted, ok := connection.(*tls.Conn)
	if !ok {
		return nil, errors.Join(errors.New("fixture did not establish TLS"), connection.Close())
	}
	return encrypted, nil
}

func TestTLSServerListenerFailureCancelsAndJoinsRequests(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, clientTLS := serverFixtureConfig(t)
	entered, joined := make(chan struct{}), make(chan struct{})
	cfg.Authenticate = func(request *http.Request) (broker.Identity, error) {
		close(entered)
		<-request.Context().Done()
		close(joined)
		return broker.Identity{}, request.Context().Err()
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url, client, stop := startBrokerServer(t, cfg, clientTLS, instance, listener)
	returned := make(chan struct{})
	go func() {
		response, _ := serverGET(t.Context(), client, url+"/v1/capabilities")
		if response != nil {
			_ = response.Body.Close()
		}
		close(returned)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach authentication")
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("listener failure did not cancel active request")
	}
	if err = stop(); !errors.Is(err, net.ErrClosed) {
		t.Fatal("listener failure hidden", err)
	}
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("closed request remained blocked")
	}
}

func TestTLSServerSingleLifetimeAndListenerOwnership(t *testing.T) {
	instance, _, _, _ := setup(t)
	cfg, _ := serverFixtureConfig(t)
	server, err := broker.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = server.Serve(ctx, first, instance); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = first.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatal("cancelled startup retained listener", err)
	}
	second, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Serve(t.Context(), second, instance); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("server lifetime reused", err)
	}
	if err = second.Close(); err != nil {
		t.Fatal("rejected reuse took listener ownership", err)
	}
}
