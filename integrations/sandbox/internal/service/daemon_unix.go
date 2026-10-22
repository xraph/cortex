//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
)

// DaemonConfig joins prepared worker services with a broker journal. Controller
// and Control must be unused and share one journal. Broker.Runtime must be nil;
// the daemon supplies the runtime adapter. Host configuration owns signing keys,
// current authorization, providers, qualification evidence and private paths.
type DaemonConfig struct {
	Controller *Controller
	Control    *WorkerControl
	Broker     broker.Config
	Attest     DeploymentAttestation
	Socket     string
	Interval   time.Duration
}

// PublicServer serves the authenticated broker until cancellation, then closes
// its listener and joins every request before returning. Configure TLS, transport
// deadlines and authentication through Broker.Handler. Never retain the broker
// beyond this call. All host callbacks must honor cancellation.
type PublicServer func(context.Context, *broker.Broker) error

// Daemon owns one broker lifetime, private socket and child-service join. The
// host retains ownership of the prepared worker journal, reaper and providers;
// close them only after Run returns. Construction does not qualify the worker.
type Daemon struct {
	cfg          DaemonConfig
	mu           sync.Mutex
	started      bool
	openEndpoint func(context.Context, string) (*controlEndpoint, error)
	serveControl func(context.Context, *net.UnixListener, func()) error
}

// NewDaemon binds public admission to successful worker recovery and private
// transport startup. The reconciliation interval is between 10ms and one second.
func NewDaemon(cfg DaemonConfig) (*Daemon, error) {
	if cfg.Controller == nil || cfg.Control == nil || cfg.Control.controller != cfg.Controller || cfg.Broker.Runtime != nil || !profilePath(cfg.Socket) || len(cfg.Socket) > 100 || cfg.Interval < 10*time.Millisecond || cfg.Interval > time.Second {
		return nil, fmt.Errorf("cortex: invalid sandbox daemon configuration")
	}
	cfg.Controller.mu.Lock()
	controllerUsed := cfg.Controller.started
	cfg.Controller.mu.Unlock()
	cfg.Control.mu.Lock()
	controlUsed := cfg.Control.started
	cfg.Control.mu.Unlock()
	if controllerUsed || controlUsed {
		return nil, broker.ErrConflict
	}
	adapter, err := NewRuntime(cfg.Controller, cfg.Attest)
	if err != nil {
		return nil, err
	}
	if len(cfg.Broker.SigningKey) != ed25519.PrivateKeySize || !bytes.Equal(cfg.Broker.SigningKey, ed25519.NewKeyFromSeed(cfg.Broker.SigningKey[:ed25519.SeedSize])) {
		return nil, fmt.Errorf("cortex: invalid sandbox broker signing key")
	}
	if identityErr := cfg.Controller.launcher.store.CheckBroker(cfg.Broker.Name, ed25519.PublicKey(cfg.Broker.SigningKey[ed25519.SeedSize:])); identityErr != nil {
		return nil, identityErr
	}
	cfg.Broker.Runtime = adapter
	cfg.Broker.SigningKey = bytes.Clone(cfg.Broker.SigningKey)
	cfg.Broker.Images = slices.Clone(cfg.Broker.Images)
	if cfg.Broker.Now == nil {
		cfg.Broker.Now = time.Now
	}
	return &Daemon{cfg: cfg, openEndpoint: func(ctx context.Context, path string) (*controlEndpoint, error) {
		if runtime.GOOS != "linux" || os.Geteuid() != 0 {
			return nil, errUnsupported
		}
		return openControlEndpoint(ctx, path, 0)
	}, serveControl: cfg.Control.serveOwned}, nil
}

// Run acquires the broker journal and private endpoint, recovers workers, starts
// private control, reconciles broker state, then admits the public server. Any
// child exit or reconciliation error cancels and joins every child. Socket and
// broker ownership are released only after that join; cleanup errors survive.
// A daemon is single-use. The host must restart with newly prepared services.
func (d *Daemon) Run(ctx context.Context, serve PublicServer) (err error) {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return broker.ErrConflict
	}
	d.started = true
	d.mu.Unlock()
	if serve == nil {
		return fmt.Errorf("cortex: missing public sandbox server")
	}
	if operationErr := ctx.Err(); operationErr != nil {
		return operationErr
	}
	lifetime, cancel := context.WithCancel(ctx)
	group := daemonGroup{results: make(chan error, 3), cancel: cancel}
	var endpoint *controlEndpoint
	var instance *broker.Broker
	defer func() {
		cancel()
		err = errors.Join(err, group.join())
		if endpoint != nil {
			err = errors.Join(err, endpoint.Close())
		}
		if instance != nil {
			err = errors.Join(err, instance.Close())
		}
	}()
	instance, err = broker.Open(d.cfg.Broker)
	if err != nil {
		return err
	}
	endpoint, err = d.openEndpoint(lifetime, d.cfg.Socket)
	if err != nil {
		return err
	}
	group.start("worker controller", func() error { return d.cfg.Controller.Run(lifetime) })
	if operationErr := d.awaitController(lifetime, &group); operationErr != nil {
		return operationErr
	}
	ready := make(chan struct{})
	group.start("worker control", func() error {
		return d.serveControl(lifetime, endpoint.listener, func() { close(ready) })
	})
	select {
	case <-lifetime.Done():
		return lifetime.Err()
	case childErr := <-group.results:
		group.running--
		return childErr
	case <-ready:
	}
	if operationErr := d.reconcile(lifetime, instance); operationErr != nil {
		return operationErr
	}
	group.start("public broker", func() error { return serve(lifetime, instance) })
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-lifetime.Done():
			return lifetime.Err()
		case childErr := <-group.results:
			group.running--
			return childErr
		case <-ticker.C:
			if operationErr := d.reconcile(lifetime, instance); operationErr != nil {
				return operationErr
			}
		}
	}
}

func (d *Daemon) awaitController(ctx context.Context, group *daemonGroup) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if d.cfg.Controller.Status().Ready {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case childErr := <-group.results:
			group.running--
			return childErr
		case <-ticker.C:
		}
	}
}

func (d *Daemon) reconcile(ctx context.Context, instance *broker.Broker) error {
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return instance.Reconcile(operation, d.cfg.Broker.Now())
}

type daemonGroup struct {
	results chan error
	running int
	cancel  context.CancelFunc
}

func (g *daemonGroup) start(name string, call func() error) {
	g.running++
	go func() {
		err := call()
		if err == nil {
			err = fmt.Errorf("cortex: sandbox %s stopped unexpectedly", name)
		} else {
			err = fmt.Errorf("cortex: sandbox %s: %w", name, err)
		}
		g.cancel()
		g.results <- err
	}()
}

func (g *daemonGroup) join() error {
	var failures []error
	for g.running > 0 {
		failures = append(failures, <-g.results)
		g.running--
	}
	return errors.Join(failures...)
}
