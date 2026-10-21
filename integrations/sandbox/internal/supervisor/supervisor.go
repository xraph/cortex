// Package supervisor owns one sandbox's bounded worker lifecycle. A durable
// outer service must claim the job before Run and kill its cgroup if this worker
// dies. This package alone does not provide crash recovery or qualification.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/xraph/cortex/sandbox"
)

var (
	// ErrOutputLimit means stdout and stderr exceeded their combined byte budget.
	ErrOutputLimit = errors.New("cortex: sandbox output limit exceeded")
	// ErrCleanup withholds terminal output until an external reaper proves stop.
	ErrCleanup = errors.New("cortex: sandbox cleanup is unresolved")
)

// Driver controls only the selected sandbox. Prepare may create processes even
// when its response is lost. Cleanup must terminate every descendant and join
// all output readers before returning nil. Operations must honor their contexts.
type Driver interface {
	Prepare(context.Context, sandbox.Job, io.Writer, io.Writer) error
	Start(context.Context) error
	Wait(context.Context) (int, error)
	Cleanup(context.Context) error
}

// Run handles one already-authorized, durably claimed job. Deadline comes from
// the accepted receipt; the job's wall limit can only shorten it. A returned
// terminal result reports execution, never correctness. Non-cleanup failures
// return both that result and an error; ErrCleanup returns no collectible result.
func Run(ctx context.Context, driver Driver, job sandbox.Job, deadline time.Time) (sandbox.Result, error) {
	if err := job.Validate(); err != nil {
		return sandbox.Result{}, err
	}
	if driver == nil || deadline.IsZero() {
		return sandbox.Result{}, fmt.Errorf("cortex: incomplete sandbox supervision")
	}
	if err := ctx.Err(); err != nil {
		return sandbox.Result{}, err
	}
	now := time.Now()
	if !now.Before(deadline) {
		return sandbox.Result{}, context.DeadlineExceeded
	}
	if maximum := now.Add(job.Limits.WallTime); deadline.After(maximum) {
		deadline = maximum
	}
	limited, release := context.WithDeadline(ctx, deadline)
	defer release()
	execution, cancel := context.WithCancelCause(limited)
	defer cancel(nil)
	output := &capture{maximum: job.Limits.OutputBytes, cancel: cancel}
	exit, reason, failure := execute(execution, driver, job, output)
	// Cancellation of the client or execution must not cancel the reap attempt.
	cleanupCtx, stopCleanup := context.WithTimeout(context.Background(), 15*time.Second)
	cleanupErr := driver.Cleanup(cleanupCtx)
	if cleanupErr == nil {
		cleanupErr = cleanupCtx.Err()
	}
	stopCleanup()
	stdout, stderr, outputErr := output.finish()
	if cleanupErr != nil {
		return sandbox.Result{}, errors.Join(ErrCleanup, failure, outputErr, cleanupErr)
	}
	state := sandbox.StateExited
	if cause := context.Cause(execution); cause != nil {
		failure = errors.Join(failure, cause)
		reason = "sandbox execution interrupted"
		if errors.Is(cause, context.Canceled) {
			state = sandbox.StateCancelled
		}
	}
	if outputErr != nil {
		failure = errors.Join(failure, outputErr)
		reason = "sandbox output limit exceeded"
		state = sandbox.StateFailed
	}
	if failure != nil && state == sandbox.StateExited {
		state = sandbox.StateFailed
	}
	if failure != nil {
		exit = -1
	}
	return sandbox.Result{JobDigest: job.Digest, State: state, ExitCode: exit, Reason: reason, Stdout: stdout, Stderr: stderr}, failure
}

func execute(ctx context.Context, driver Driver, job sandbox.Job, output *capture) (exitCode int, reason string, failure error) {
	if err := driver.Prepare(ctx, job, &stream{capture: output}, &stream{capture: output, diagnostic: true}); err != nil {
		return -1, "sandbox preparation failed", err
	}
	if err := context.Cause(ctx); err != nil {
		return -1, "sandbox preparation interrupted", err
	}
	if err := driver.Start(ctx); err != nil {
		return -1, "sandbox start failed", err
	}
	if err := context.Cause(ctx); err != nil {
		return -1, "sandbox start interrupted", err
	}
	code, err := driver.Wait(ctx)
	if err != nil {
		return -1, "sandbox wait failed", err
	}
	if code < 0 || code > 255 {
		return -1, "invalid sandbox exit status", fmt.Errorf("cortex: invalid sandbox exit status")
	}
	return code, "sandbox exited", nil
}

type capture struct {
	mu             sync.Mutex
	maximum        int64
	stdout, stderr []byte
	cancel         context.CancelCauseFunc
	failure        error
	closed         bool
}

type stream struct {
	*capture
	diagnostic bool
}

func (s *stream) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	if s.failure != nil {
		return 0, s.failure
	}
	n := len(data)
	remaining := int(s.maximum) - len(s.stdout) - len(s.stderr)
	if n > remaining {
		n = remaining
	}
	if s.diagnostic {
		s.stderr = append(s.stderr, data[:n]...)
	} else {
		s.stdout = append(s.stdout, data[:n]...)
	}
	if n != len(data) {
		s.failure = ErrOutputLimit
		s.cancel(ErrOutputLimit)
	}
	return n, s.failure
}

func (c *capture) finish() (stdout, stderr []byte, failure error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return append([]byte(nil), c.stdout...), append([]byte(nil), c.stderr...), c.failure
}
