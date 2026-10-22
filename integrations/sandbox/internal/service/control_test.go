package service

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

func TestControlPreservesAuthenticatedActivationPending(t *testing.T) {
	path, cancel, done := startControl(t, func(context.Context, *net.UnixConn, WorkerClaim) error {
		return errWorkerActivating
	}, ControlLimits{Connections: 1, Timeout: time.Second})
	if err := authorizeConnection(t.Context(), dialControl(t, path), controlClaim(), time.Second); !errors.Is(err, broker.ErrPending) {
		t.Fatalf("activation pending was lost: %v", err)
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestControlProviderPendingDoesNotPermitStartupRetry(t *testing.T) {
	for _, failure := range []error{broker.ErrPending, errors.Join(errWorkerActivating, errors.New("peer cleanup failed"))} {
		path, cancel, done := startControl(t, func(context.Context, *net.UnixConn, WorkerClaim) error {
			return failure
		}, ControlLimits{Connections: 1, Timeout: time.Second})
		if err := authorizeConnection(t.Context(), dialControl(t, path), controlClaim(), time.Second); !errors.Is(err, broker.ErrDenied) {
			t.Fatalf("provider or cleanup failure was treated as worker activation: %v", err)
		}
		cancel()
		_ = awaitControllerStop(t, done)
	}
}

func controlListener(t *testing.T) (*net.UnixListener, string) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "cx-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return listener, path
}

func controlClaim() WorkerClaim {
	return WorkerClaim{SchemaVersion: 1, JobID: id.New(sandbox.PrefixJob), Token: strings.Repeat("a", 64), Revision: 2, ProfileDigest: evidence.Digest([]byte("profile"))}
}

func startControl(t *testing.T, authorize func(context.Context, *net.UnixConn, WorkerClaim) error, limits ControlLimits) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, path := controlListener(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	server := &WorkerControl{limits: limits, authorize: authorize}
	go func() { done <- server.serve(ctx, listener) }()
	return path, cancel, done
}

func dialControl(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err = conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestControlAuthorizesOneBoundClaim(t *testing.T) {
	claim := controlClaim()
	var calls atomic.Int32
	path, cancel, done := startControl(t, func(_ context.Context, _ *net.UnixConn, got WorkerClaim) error {
		calls.Add(1)
		if got != claim {
			return broker.ErrDenied
		}
		return nil
	}, ControlLimits{Connections: 2, Timeout: time.Second})
	if err := authorizeConnection(t.Context(), dialControl(t, path), claim, time.Second); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("authorization not called exactly once")
	}
	cancel()
	if err := awaitControllerStop(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestControlRejectsMalformedRequestsBeforeAuthority(t *testing.T) {
	claim := controlClaim()
	body, _ := json.Marshal(claim)
	for _, test := range []struct {
		name   string
		body   []byte
		length uint32
	}{
		{"empty", nil, 0}, {"oversized", nil, 2049}, {"truncated", body[:3], uint32(len(body))},
		{"unknown", []byte(strings.TrimSuffix(string(body), "}") + `,"unexpected":true}`), 0},
		{"duplicate", []byte(strings.TrimSuffix(string(body), "}") + `,"schema_version":1}`), 0},
		{"whitespace", append([]byte(" "), body...), 0},
		{"two frames", append(append([]byte(nil), body...), body...), uint32(len(body))},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			path, cancel, done := startControl(t, func(context.Context, *net.UnixConn, WorkerClaim) error { calls.Add(1); return nil }, ControlLimits{Connections: 1, Timeout: time.Second})
			conn := dialControl(t, path)
			n := test.length
			if n == 0 {
				n = uint32(len(test.body))
			}
			var header [4]byte
			binary.BigEndian.PutUint32(header[:], n)
			_, _ = conn.Write(append(header[:], test.body...))
			_ = conn.CloseWrite()
			var reply [1024]byte
			count, _ := conn.Read(reply[:])
			if calls.Load() != 0 || strings.Contains(string(reply[:count]), claim.Token) {
				t.Fatal("malformed request reached authority or exposed token")
			}
			cancel()
			_ = awaitControllerStop(t, done)
		})
	}
}

func TestControlSanitizesDenialsAndJoinsShutdown(t *testing.T) {
	claim := controlClaim()
	path, cancel, done := startControl(t, func(context.Context, *net.UnixConn, WorkerClaim) error { return errors.New(claim.Token) }, ControlLimits{Connections: 1, Timeout: time.Second})
	err := authorizeConnection(t.Context(), dialControl(t, path), claim, time.Second)
	if !errors.Is(err, broker.ErrDenied) || strings.Contains(err.Error(), claim.Token) {
		t.Fatalf("unsafe denial: %v", err)
	}
	cancel()
	_ = awaitControllerStop(t, done)
	entered := make(chan struct{})
	exited := make(chan struct{})
	path, cancel, done = startControl(t, func(ctx context.Context, _ *net.UnixConn, _ WorkerClaim) error {
		close(entered)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}, ControlLimits{Connections: 1, Timeout: time.Second})
	conn := dialControl(t, path)
	result := make(chan error, 1)
	go func() { result <- authorizeConnection(t.Context(), conn, claim, time.Second) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request not admitted")
	}
	cancel()
	_ = awaitControllerStop(t, done)
	select {
	case <-exited:
	default:
		t.Fatal("handler outlived shutdown")
	}
	if err = awaitControllerStop(t, result); err == nil {
		t.Fatal("shutdown authorized worker")
	}
}

func TestControlBoundsConcurrentAuthorization(t *testing.T) {
	var active, peak atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	path, cancel, done := startControl(t, func(ctx context.Context, _ *net.UnixConn, _ WorkerClaim) error {
		current := active.Add(1)
		defer active.Add(-1)
		for prior := peak.Load(); current > prior; prior = peak.Load() {
			if peak.CompareAndSwap(prior, current) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, ControlLimits{Connections: 2, Timeout: 2 * time.Second})
	results := make(chan error, 6)
	for range 6 {
		conn := dialControl(t, path)
		go func() { results <- authorizeConnection(t.Context(), conn, controlClaim(), 2*time.Second) }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("connections not admitted")
		}
	}
	select {
	case <-entered:
		t.Fatal("connection ceiling exceeded")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for range 6 {
		if err := awaitControllerStop(t, results); err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("unexpected peak %d", peak.Load())
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestControlTimesOutStalledFramesAndAuthorization(t *testing.T) {
	for _, mode := range []string{"header", "body", "missing EOF", "authorization"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			returned := make(chan struct{}, 1)
			path, cancel, done := startControl(t, func(ctx context.Context, _ *net.UnixConn, _ WorkerClaim) error {
				calls.Add(1)
				<-ctx.Done()
				returned <- struct{}{}
				return ctx.Err()
			}, ControlLimits{Connections: 1, Timeout: 50 * time.Millisecond})
			conn := dialControl(t, path)
			switch mode {
			case "header":
				_, _ = conn.Write([]byte{0})
			case "body":
				_, _ = conn.Write([]byte{0, 0, 1, 0, '{'})
			default:
				if err := writeControlFrame(conn, controlClaim()); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "authorization" {
				_ = conn.CloseWrite()
			}
			var reply [256]byte
			n, err := conn.Read(reply[:])
			if n != 0 || err == nil {
				t.Fatal("stalled request retained connection")
			}
			if mode == "authorization" {
				select {
				case <-returned:
				case <-time.After(time.Second):
					t.Fatal("authorization was not canceled")
				}
			} else if calls.Load() != 0 {
				t.Fatal("incomplete request reached authorization")
			}
			cancel()
			_ = awaitControllerStop(t, done)
		})
	}
}

func TestControlClientRejectsForgedOrUnboundedReplies(t *testing.T) {
	claim := controlClaim()
	body, _ := json.Marshal(claim)
	valid := controlReply{SchemaVersion: 1, Status: "authorized", ClaimDigest: evidence.Digest(body)}
	for _, mode := range []string{"different claim", "unbound pending", "wrong schema", "unknown status", "trailing data", "oversized", "truncated", "no EOF", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			listener, path := controlListener(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.AcceptUnix()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				var got WorkerClaim
				if _, err = readControlFrame(conn, &got); err != nil {
					return
				}
				if err = requireControlEOF(conn); err != nil {
					return
				}
				reply := valid
				switch mode {
				case "different claim":
					reply.ClaimDigest = evidence.Digest([]byte("different"))
				case "unbound pending":
					reply.Status, reply.ClaimDigest = "pending", evidence.Digest([]byte("different"))
				case "wrong schema":
					reply.SchemaVersion++
				case "unknown status":
					reply.Status = "completed"
				case "oversized":
					_, _ = conn.Write([]byte{0, 0, 8, 1})
					return
				case "truncated":
					_, _ = conn.Write([]byte{0, 0, 0, 10, '{'})
					return
				case "canceled":
					cancel()
					return
				}
				_ = writeControlFrame(conn, reply)
				if mode == "trailing data" {
					_, _ = conn.Write([]byte{1})
				}
				if mode == "no EOF" {
					var extra [1]byte
					_, _ = conn.Read(extra[:])
					time.Sleep(100 * time.Millisecond)
				}
			}()
			if err := authorizeConnection(ctx, dialControl(t, path), claim, 50*time.Millisecond); err == nil || errors.Is(err, broker.ErrPending) {
				t.Fatal("invalid reply authorized worker or permitted activation retry")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("reply fixture leaked")
			}
		})
	}
}

func TestControlUsesCurrentControllerAuthority(t *testing.T) {
	controller, claim, peer, _ := authorizationFixture(t)
	path, cancel, done := startControl(t, func(ctx context.Context, _ *net.UnixConn, request WorkerClaim) error {
		_, err := controller.authorizeWorker(ctx, peer, request)
		return err
	}, ControlLimits{Connections: 2, Timeout: time.Second})
	if err := authorizeConnection(t.Context(), dialControl(t, path), claim, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.launcher.store.Cancel(t.Context(), claim.JobID); err != nil {
		t.Fatal(err)
	}
	if err := authorizeConnection(t.Context(), dialControl(t, path), claim, time.Second); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("stale claim authorized: %v", err)
	}
	cancel()
	_ = awaitControllerStop(t, done)
}

func TestControlRejectsInvalidLimitsAndReuse(t *testing.T) {
	for _, limits := range []ControlLimits{{}, {Connections: 65, Timeout: time.Second}, {Connections: 1, Timeout: time.Millisecond}, {Connections: 1, Timeout: 31 * time.Second}} {
		if limits.validate() == nil {
			t.Fatal("invalid resource bounds accepted")
		}
	}
	listener, _ := controlListener(t)
	server := &WorkerControl{limits: ControlLimits{Connections: 1, Timeout: time.Second}, authorize: func(context.Context, *net.UnixConn, WorkerClaim) error { return nil }}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := server.serve(ctx, listener); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := server.serve(t.Context(), listener); !errors.Is(err, broker.ErrConflict) {
		t.Fatal("control server reused")
	}
}
