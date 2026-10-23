package service

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

func TestDaemonTLSBrokerJoinsServingBeforeJournalRelease(t *testing.T) {
	daemon, job, _ := daemonFixture(t)
	certificateFixture := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := certificateFixture.TLS.Certificates[0]
	transport := certificateFixture.Client().Transport.(*http.Transport).Clone()
	certificateFixture.Close()
	defer transport.CloseIdleConnections()
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS fixture does not verify server identity")
	}
	public, err := broker.NewServer(broker.ServerConfig{Certificate: certificate, Authenticate: func(*http.Request) (broker.Identity, error) {
		return broker.Identity{Scope: job.Scope, Principal: job.Principal}, nil
	}, Limits: broker.ServerLimits{Connections: 4, HeaderBytes: 4096, HeaderTimeout: time.Second, RequestTimeout: 5 * time.Second, IdleTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	key := ed25519.PublicKey(daemon.cfg.Broker.SigningKey[ed25519.SeedSize:])
	client, err := broker.NewClient("https://"+listener.Addr().String(), &http.Client{Transport: transport, Timeout: 3 * time.Second}, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, func(lifetime context.Context, instance *broker.Broker) error {
			return public.Serve(lifetime, listener, instance)
		})
	}()
	receipt, err := client.Submit(t.Context(), job)
	if err != nil {
		t.Fatal("TLS broker did not admit after startup", err)
	}
	if err = broker.VerifyReceipt(receipt, key); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if connection, acceptErr := listener.Accept(); !errors.Is(acceptErr, net.ErrClosed) {
		if connection != nil {
			_ = connection.Close()
		}
		t.Fatal("public listener retained", acceptErr)
	}
	record, err := daemon.cfg.Controller.launcher.store.Load(t.Context(), job.ID)
	if err != nil || record.Phase != workerstore.Terminal || record.Result.State != sandbox.StateFailed {
		t.Fatal("daemon returned before worker recovery", err)
	}
	reopened, err := broker.Open(daemon.cfg.Broker)
	if err != nil {
		t.Fatal("broker journal ownership retained", err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
