package sandboxbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// ServerLimits bounds connections, HTTP headers and each transport phase.
// HTTP/1.1 permits at most one active broker request per connection.
type ServerLimits struct {
	Connections, HeaderBytes                   int
	HeaderTimeout, RequestTimeout, IdleTimeout time.Duration
}

func (l ServerLimits) validate() error {
	if l.Connections < 1 || l.Connections > 256 || l.HeaderBytes < 1024 || l.HeaderBytes > 32<<10 || l.HeaderTimeout < 10*time.Millisecond || l.HeaderTimeout > 10*time.Second || l.RequestTimeout < 50*time.Millisecond || l.RequestTimeout > 30*time.Second || l.HeaderTimeout > l.RequestTimeout || l.IdleTimeout < 10*time.Millisecond || l.IdleTimeout > 30*time.Second {
		return fmt.Errorf("cortex: invalid sandbox HTTP limits")
	}
	return nil
}

// ServerConfig uses static certificate material and current host authentication.
// A non-nil ClientCAs requires a verified client certificate before Authenticate.
// Otherwise Authenticate must validate host-issued credentials over TLS. All
// authentication, authorization and runtime callbacks must honor their contexts.
type ServerConfig struct {
	Certificate  tls.Certificate
	ClientCAs    *x509.CertPool
	Authenticate Authenticator
	Limits       ServerLimits
}

// Server owns one bounded TLS listener lifetime. It exposes only Broker.Handler,
// disables HTTP multiplexing and closes each expired request's connection.
// Construction copies certificate/key material and the client trust pool.
type Server struct {
	tls          *tls.Config
	authenticate Authenticator
	limits       ServerLimits
	mu           sync.Mutex
	started      bool
}

// NewServer requires explicit finite limits, a static certificate and host
// authentication. TLS 1.3 is required; dynamic TLS policy callbacks are not used.
func NewServer(cfg ServerConfig) (*Server, error) {
	if err := cfg.Limits.validate(); err != nil {
		return nil, err
	}
	if cfg.Authenticate == nil {
		return nil, fmt.Errorf("cortex: missing sandbox HTTP authentication")
	}
	certificate, err := freezeCertificate(cfg.Certificate)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, NextProtos: []string{"http/1.1"}, SessionTicketsDisabled: true}
	if cfg.ClientCAs != nil {
		config.ClientCAs = cfg.ClientCAs.Clone()
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return &Server{tls: config, authenticate: cfg.Authenticate, limits: cfg.Limits}, nil
}

// Serve takes ownership of listener on its first valid call. Cancellation or a
// listener failure closes every connection, cancels and joins broker handlers,
// then returns. The caller owns the broker journal and closes it afterward.
// Later calls return ErrConflict without taking ownership of their listeners.
func (s *Server) Serve(ctx context.Context, listener net.Listener, instance *Broker) error {
	if listener == nil || instance == nil {
		return fmt.Errorf("cortex: missing sandbox HTTP listener or broker")
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrConflict
	}
	s.started = true
	s.mu.Unlock()
	defer listener.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	requests := &serverRequests{accepting: true, slots: make(chan struct{}, s.limits.Connections)}
	handler := instance.Handler(s.authenticate)
	server := &http.Server{
		TLSConfig:         s.tls,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ReadHeaderTimeout: s.limits.HeaderTimeout,
		ReadTimeout:       s.limits.RequestTimeout,
		WriteTimeout:      s.limits.RequestTimeout,
		IdleTimeout:       s.limits.IdleTimeout,
		MaxHeaderBytes:    s.limits.HeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return lifetime },
		ConnContext: func(parent context.Context, connection net.Conn) context.Context {
			if encrypted, ok := connection.(*tls.Conn); ok {
				connection = encrypted.NetConn()
			}
			return context.WithValue(parent, serverConnectionKey{}, connection)
		},
		ErrorLog: log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !requests.begin() {
				http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
				return
			}
			defer requests.end()
			operation, finish := context.WithTimeout(r.Context(), s.limits.RequestTimeout)
			connection, ok := r.Context().Value(serverConnectionKey{}).(net.Conn)
			if !ok {
				finish()
				http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
				return
			}
			closed := make(chan struct{})
			stop := context.AfterFunc(operation, func() { _ = connection.Close(); close(closed) })
			defer func() {
				stopped := stop()
				finish()
				if !stopped {
					<-closed
				}
			}()
			if operation.Err() != nil {
				return
			}
			handler.ServeHTTP(w, r.WithContext(operation))
		}),
	}
	bounded := &serverListener{Listener: listener, slots: make(chan struct{}, s.limits.Connections), closed: make(chan struct{})}
	var closeErr error
	shutdown := sync.OnceFunc(func() { requests.stop(); cancel(); closeErr = server.Close() })
	closed := make(chan struct{})
	stop := context.AfterFunc(lifetime, func() { shutdown(); close(closed) })
	serveErr := server.ServeTLS(bounded, "", "")
	shutdown()
	if !stop() {
		<-closed
	}
	requests.active.Wait()
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, closeErr, ctx.Err())
}

type serverConnectionKey struct{}

type serverRequests struct {
	mu        sync.Mutex
	accepting bool
	active    sync.WaitGroup
	slots     chan struct{}
}

func (r *serverRequests) begin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.accepting {
		return false
	}
	select {
	case r.slots <- struct{}{}:
	default:
		return false
	}
	r.active.Add(1)
	return true
}
func (r *serverRequests) end()  { <-r.slots; r.active.Done() }
func (r *serverRequests) stop() { r.mu.Lock(); r.accepting = false; r.mu.Unlock() }

type serverListener struct {
	net.Listener
	slots       chan struct{}
	closed      chan struct{}
	once        sync.Once
	err         error
	mu          sync.Mutex
	stopped     bool
	connections map[*serverConnection]struct{}
}

func (l *serverListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}
	connection, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	bounded := &serverConnection{Conn: connection}
	bounded.release = func() {
		l.mu.Lock()
		delete(l.connections, bounded)
		l.mu.Unlock()
		<-l.slots
	}
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return nil, errors.Join(net.ErrClosed, bounded.Close())
	}
	if l.connections == nil {
		l.connections = make(map[*serverConnection]struct{})
	}
	l.connections[bounded] = struct{}{}
	l.mu.Unlock()
	return bounded, nil
}
func (l *serverListener) Close() error {
	l.once.Do(func() {
		l.mu.Lock()
		l.stopped = true
		connections := make([]*serverConnection, 0, len(l.connections))
		for connection := range l.connections {
			connections = append(connections, connection)
		}
		l.mu.Unlock()
		close(l.closed)
		l.err = l.Listener.Close()
		for _, connection := range connections {
			l.err = errors.Join(l.err, connection.Close())
		}
	})
	return l.err
}

type serverConnection struct {
	net.Conn
	release func()
	once    sync.Once
	err     error
}

func (c *serverConnection) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close(); c.release() })
	return c.err
}
