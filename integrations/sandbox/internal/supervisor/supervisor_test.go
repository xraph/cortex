package supervisor

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

type fixtureDriver struct {
	prepare func(context.Context, io.Writer, io.Writer) error
	start   func(context.Context) error
	wait    func(context.Context) (int, error)
	cleanup func(context.Context) error
	calls   []string
}

func (d *fixtureDriver) Prepare(ctx context.Context, _ sandbox.Job, out, diagnostic io.Writer) error {
	d.calls = append(d.calls, "prepare")
	if d.prepare != nil {
		return d.prepare(ctx, out, diagnostic)
	}
	return nil
}
func (d *fixtureDriver) Start(ctx context.Context) error {
	d.calls = append(d.calls, "start")
	if d.start != nil {
		return d.start(ctx)
	}
	return nil
}
func (d *fixtureDriver) Wait(ctx context.Context) (int, error) {
	d.calls = append(d.calls, "wait")
	if d.wait != nil {
		return d.wait(ctx)
	}
	return 0, nil
}
func (d *fixtureDriver) Cleanup(ctx context.Context) error {
	d.calls = append(d.calls, "cleanup")
	if d.cleanup != nil {
		return d.cleanup(ctx)
	}
	return nil
}
func job(t *testing.T) sandbox.Job {
	t.Helper()
	j, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "supervisor", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, Principal: "host", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/bin/check"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 500, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 8 << 20, WallTime: time.Second, OutputBytes: 8}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestSuccessfulExitRequiresCleanupAndPreservesOutput(t *testing.T) {
	j := job(t)
	d := &fixtureDriver{prepare: func(_ context.Context, out, diagnostic io.Writer) error {
		if _, err := out.Write([]byte("hello")); err != nil {
			return err
		}
		_, err := diagnostic.Write([]byte("err"))
		return err
	}}
	r, err := Run(t.Context(), d, j, time.Now().Add(time.Second))
	if err != nil || r.State != sandbox.StateExited || r.ExitCode != 0 || r.JobDigest != j.Digest || string(r.Stdout) != "hello" || string(r.Stderr) != "err" {
		t.Fatalf("result=%+v error=%v", r, err)
	}
	if strings.Join(d.calls, ",") != "prepare,start,wait,cleanup" {
		t.Fatalf("lifecycle=%v", d.calls)
	}
}
func TestAmbiguousRuntimeFailuresAlwaysCleanUp(t *testing.T) {
	failure := errors.New("uncertain runtime acknowledgement")
	for _, stage := range []string{"prepare", "start", "wait"} {
		t.Run(stage, func(t *testing.T) {
			d := &fixtureDriver{}
			switch stage {
			case "prepare":
				d.prepare = func(context.Context, io.Writer, io.Writer) error { return failure }
			case "start":
				d.start = func(context.Context) error { return failure }
			case "wait":
				d.wait = func(context.Context) (int, error) { return 0, failure }
			}
			r, err := Run(t.Context(), d, job(t), time.Now().Add(time.Second))
			if !errors.Is(err, failure) || r.State != sandbox.StateFailed || d.calls[len(d.calls)-1] != "cleanup" {
				t.Fatalf("result=%+v error=%v calls=%v", r, err, d.calls)
			}
			if stage == "prepare" && len(d.calls) != 2 {
				t.Fatal("started after failed prepare")
			}
		})
	}
}
func TestOutputLimitIsSharedAndStopsBeforeStart(t *testing.T) {
	d := &fixtureDriver{prepare: func(_ context.Context, out, diagnostic io.Writer) error {
		if _, err := out.Write([]byte("123456")); err != nil {
			return err
		}
		_, err := diagnostic.Write([]byte("abcdef"))
		return err
	}}
	r, err := Run(t.Context(), d, job(t), time.Now().Add(time.Second))
	if !errors.Is(err, ErrOutputLimit) || r.State != sandbox.StateFailed || string(r.Stdout) != "123456" || string(r.Stderr) != "ab" {
		t.Fatalf("result=%+v error=%v", r, err)
	}
	if strings.Join(d.calls, ",") != "prepare,cleanup" {
		t.Fatalf("started after output overflow: %v", d.calls)
	}
}
func TestConcurrentOutputCannotExceedCombinedBudget(t *testing.T) {
	d := &fixtureDriver{prepare: func(_ context.Context, out, diagnostic io.Writer) error {
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Go(func() { _, _ = out.Write([]byte("abcd")); _, _ = diagnostic.Write([]byte("efgh")) })
		}
		wg.Wait()
		return nil
	}}
	r, err := Run(t.Context(), d, job(t), time.Now().Add(time.Second))
	if !errors.Is(err, ErrOutputLimit) || len(r.Stdout)+len(r.Stderr) != 8 {
		t.Fatalf("result=%+v error=%v", r, err)
	}
}
func TestCleanupHasIndependentDeadlineAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &fixtureDriver{start: func(context.Context) error { cancel(); return nil }, cleanup: func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("cleanup inherited cancellation")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("cleanup is unbounded")
		}
		return nil
	}}
	r, err := Run(ctx, d, job(t), time.Now().Add(time.Second))
	if !errors.Is(err, context.Canceled) || r.State != sandbox.StateCancelled || strings.Join(d.calls, ",") != "prepare,start,cleanup" {
		t.Fatalf("result=%+v error=%v calls=%v", r, err, d.calls)
	}
}
func TestCleanupFailureWithholdsTerminalResult(t *testing.T) {
	failure := errors.New("descendants still present")
	d := &fixtureDriver{cleanup: func(context.Context) error { return failure }}
	r, err := Run(t.Context(), d, job(t), time.Now().Add(time.Second))
	if !errors.Is(err, ErrCleanup) || !errors.Is(err, failure) || r.State != "" || r.JobDigest != "" {
		t.Fatalf("collectible result after failed cleanup: %+v %v", r, err)
	}
}
func TestDeadlineAndExpiredAdmission(t *testing.T) {
	j := job(t)
	d := &fixtureDriver{wait: func(ctx context.Context) (int, error) { <-ctx.Done(); return 0, ctx.Err() }}
	r, err := Run(t.Context(), d, j, time.Now().Add(20*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) || r.State != sandbox.StateFailed || d.calls[len(d.calls)-1] != "cleanup" {
		t.Fatalf("deadline result=%+v %v", r, err)
	}
	d = &fixtureDriver{}
	if _, err = Run(t.Context(), d, j, time.Now().Add(-time.Second)); !errors.Is(err, context.DeadlineExceeded) || len(d.calls) != 0 {
		t.Fatalf("expired admission: %v %v", err, d.calls)
	}
}
func TestLateOutputAndCancellationCannotBecomeSuccessfulExit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &fixtureDriver{wait: func(context.Context) (int, error) { cancel(); return 0, nil }}
	r, err := Run(ctx, d, job(t), time.Now().Add(time.Second))
	if r.State != sandbox.StateCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation ignored: %+v %v", r, err)
	}
	var output io.Writer
	d = &fixtureDriver{prepare: func(_ context.Context, out, _ io.Writer) error { output = out; return nil }, cleanup: func(context.Context) error { _, _ = output.Write([]byte("overflows output")); return nil }}
	r, err = Run(t.Context(), d, job(t), time.Now().Add(time.Second))
	if r.State != sandbox.StateFailed || !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("late output ignored: %+v %v", r, err)
	}
}
