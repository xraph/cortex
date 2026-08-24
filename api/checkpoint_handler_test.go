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

// TestDecisionFromRequest_ApprovedAndRejectedAreNotInterchangeable pins
// which wire string means which decision.
//
// Nothing else does. Every other test here goes through the invalid
// branch, so swapping the two valid cases used to leave the whole suite
// green while the endpoint approved what an operator rejected and failed
// a live run on what they approved. The decision now moves a run either
// way, so the mapping is worth an assertion of its own.
func TestDecisionFromRequest_ApprovedAndRejectedAreNotInterchangeable(t *testing.T) {
	tests := []struct {
		name     string
		decision string
		want     bool
	}{
		{name: "approved grants the call", decision: approvedDecision, want: true},
		{name: "rejected fails the run", decision: rejectedDecision, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decisionFromRequest(&ResolveCheckpointRequest{
				CheckpointID: id.NewCheckpointID().String(),
				Decision:     tc.decision,
				DecidedBy:    "ops@example.com",
				Reason:       "checked the ticket",
			})
			if err != nil {
				t.Fatalf("decisionFromRequest(%q): %v", tc.decision, err)
			}
			if got.Approved != tc.want {
				t.Errorf("decisionFromRequest(%q).Approved = %v, want %v", tc.decision, got.Approved, tc.want)
			}
			if got.DecidedBy != "ops@example.com" {
				t.Errorf("DecidedBy = %q, want the decider carried through", got.DecidedBy)
			}
			if got.Reason != "checked the ticket" {
				t.Errorf("Reason = %q, want the reason carried through; a rejection puts it on the run", got.Reason)
			}
		})
	}
}
