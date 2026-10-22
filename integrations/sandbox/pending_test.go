package sandboxbroker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

type pendingRuntime struct {
	*fixtureRuntime
	inspection error
}

func (r *pendingRuntime) Inspect(context.Context, id.ID) (sandbox.Result, error) {
	return sandbox.Result{}, r.inspection
}

func TestPendingObservationDoesNotHideRuntimeFailure(t *testing.T) {
	for _, state := range []string{"accepted", "joined", "running", "cancelling"} {
		t.Run(state, func(t *testing.T) {
			previous, runtime, cfg, ctx := setup(t)
			if err := previous.Close(); err != nil {
				t.Fatal(err)
			}
			adapter := &pendingRuntime{fixtureRuntime: runtime, inspection: broker.ErrPending}
			if state == "joined" {
				adapter.inspection = errors.Join(broker.ErrPending, errors.New("worker journal unavailable"))
			}
			cfg.Runtime = adapter
			instance, err := broker.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			if state != "running" {
				runtime.startAbsent = true
				runtime.startErr = errors.New("admission unavailable")
			}
			task := job(t)
			_, _ = instance.Submit(ctx, task)
			if state == "cancelling" {
				if err = instance.Cancel(ctx, task.ID); !errors.Is(err, broker.ErrPending) {
					t.Fatal("pending cleanup acknowledged", err)
				}
			}
			err = instance.Reconcile(t.Context(), time.Now())
			if state == "accepted" {
				if err != nil {
					t.Fatal("pending admission treated as failure", err)
				}
				status, inspectErr := instance.Inspect(ctx, task.ID)
				if inspectErr != nil || status.State != sandbox.StateAccepted {
					t.Fatal("pending admission changed state", inspectErr)
				}
			} else if !errors.Is(err, broker.ErrPending) {
				t.Fatal("runtime failure hidden", err)
			}
		})
	}
}
