package engine

import (
	"context"
	"testing"

	"github.com/xraph/cortex/id"
)

// deniedToolRecorder records every OnToolDenied call it receives.
type deniedToolRecorder struct {
	calls []string
}

func (r *deniedToolRecorder) Name() string { return "denied-tool-recorder" }

func (r *deniedToolRecorder) OnToolDenied(_ context.Context, _ id.AgentRunID, toolName, _ string) error {
	r.calls = append(r.calls, toolName)
	return nil
}

func TestEmitToolDenied_RecordsExactlyOneDenial(t *testing.T) {
	rec := &deniedToolRecorder{}
	e, err := New(WithExtension(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	e.Extensions().EmitToolDenied(context.Background(), id.NewAgentRunID(), "delete_everything", "not authorized")

	if len(rec.calls) != 1 {
		t.Fatalf("OnToolDenied called %d times, want 1", len(rec.calls))
	}
	if rec.calls[0] != "delete_everything" {
		t.Errorf("denied tool = %q, want %q", rec.calls[0], "delete_everything")
	}
}
