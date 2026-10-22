package sandboxbroker_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

type absenceRuntime struct {
	*fixtureRuntime
	inspectErr, cancelErr error
}

func (r *absenceRuntime) Inspect(context.Context, id.ID) (sandbox.Result, error) {
	return sandbox.Result{}, r.inspectErr
}
func (r *absenceRuntime) Cancel(context.Context, id.ID) error { return r.cancelErr }

func TestRuntimeAbsenceMustNotHideCleanupOrObservationFailure(t *testing.T) {
	for _, form := range []string{"joined", "wrapped"} {
		for _, operation := range []string{"inspect", "cancel", "collect", "reconcile"} {
			t.Run(form+"/"+operation, func(t *testing.T) {
				previous, runtime, cfg, ctx := setup(t)
				if err := previous.Close(); err != nil {
					t.Fatal(err)
				}
				failure := errors.New("containment observation failed")
				observationErr := errors.Join(broker.ErrNotFound, failure)
				if form == "wrapped" {
					failure = fmt.Errorf("containment observation uncertain: %w", broker.ErrNotFound)
					observationErr = failure
				}
				adapter := &absenceRuntime{fixtureRuntime: runtime, inspectErr: observationErr, cancelErr: broker.ErrNotFound}
				if operation == "cancel" {
					adapter.inspectErr = broker.ErrNotFound
					adapter.cancelErr = observationErr
				}
				cfg.Runtime = adapter
				instance, err := broker.Open(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer instance.Close()
				runtime.startAbsent = true
				runtime.startErr = errors.New("lost start response")
				task := job(t)
				if _, err = instance.Submit(ctx, task); err == nil {
					t.Fatal("ambiguous start acknowledged")
				}
				switch operation {
				case "inspect":
					_, err = instance.Inspect(ctx, task.ID)
				case "cancel":
					err = instance.Cancel(ctx, task.ID)
				case "collect":
					// Persist cancellation intent while the containment observation is uncertain.
					err = instance.Cancel(ctx, task.ID)
					if !errors.Is(err, failure) {
						t.Fatal("uncertain cancellation committed", err)
					}
					_, err = instance.Collect(ctx, task.ID)
				case "reconcile":
					err = instance.Reconcile(t.Context(), time.Now())
				}
				if !errors.Is(err, failure) {
					t.Fatal("runtime failure became absence", err)
				}
				if _, collectErr := instance.Collect(ctx, task.ID); !errors.Is(collectErr, failure) {
					t.Fatal("uncertain absence persisted a terminal result", collectErr)
				}
			})
		}
	}
}
