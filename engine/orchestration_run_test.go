package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
)

func TestRunOrchestrationNoStore(t *testing.T) {
	e, err := engine.New()
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	_, err = e.RunOrchestration(context.Background(), "app1", "team", "go")
	if !errors.Is(err, cortex.ErrNoStore) {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
}
