package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestOrchestrationCRUDNoStore(t *testing.T) {
	e, err := engine.New()
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx := context.Background()

	if err := e.CreateOrchestration(ctx, &orchestration.Config{}); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("CreateOrchestration err = %v, want ErrNoStore", err)
	}
	if _, err := e.GetOrchestration(ctx, id.NewOrchestrationConfigID()); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("GetOrchestration err = %v, want ErrNoStore", err)
	}
	if _, err := e.ListOrchestrations(ctx, nil); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("ListOrchestrations err = %v, want ErrNoStore", err)
	}
	if _, err := e.GetOrchestrationRun(ctx, id.NewOrchestrationID()); !errors.Is(err, cortex.ErrNoStore) {
		t.Errorf("GetOrchestrationRun err = %v, want ErrNoStore", err)
	}
}
