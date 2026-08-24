package api

import (
	"net/http"
	"testing"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/id"
)

// unimplementedContext is a type alias, not a field name, so embedding it
// below doesn't shadow forge.Context's own Context() method the way
// embedding forge.Context directly would.
type unimplementedContext = forge.Context

// fakeContext satisfies forge.Context without implementing anything: the
// invalid-decision path in resolveCheckpoint returns before touching any
// context method, so a context that panics on first use is exactly the
// right double here, since a call to it would itself be worth catching.
type fakeContext struct {
	unimplementedContext
}

func TestResolveCheckpoint_UnrecognisedDecisionIsRejected(t *testing.T) {
	tests := []struct {
		name     string
		decision string
	}{
		{name: "typo missing the trailing d", decision: "approve"},
		{name: "empty string", decision: ""},
		{name: "wrong case", decision: "Approved"},
		{name: "unrelated word", decision: "maybe"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &API{}
			req := &ResolveCheckpointRequest{
				CheckpointID: id.NewCheckpointID().String(),
				Decision:     tc.decision,
			}

			_, err := a.resolveCheckpoint(fakeContext{}, req)
			if err == nil {
				t.Fatalf("resolveCheckpoint(decision=%q) = nil error, want an error: an unrecognised decision must not be silently read as a rejection", tc.decision)
			}
			sc, ok := err.(statusCoder)
			if !ok {
				t.Fatalf("resolveCheckpoint(decision=%q) error = %T, want an error exposing StatusCode()", tc.decision, err)
			}
			if sc.StatusCode() != http.StatusBadRequest {
				t.Errorf("resolveCheckpoint(decision=%q) status = %d, want %d", tc.decision, sc.StatusCode(), http.StatusBadRequest)
			}
		})
	}
}
