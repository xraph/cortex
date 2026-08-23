package suspension_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/suspension"
)

func TestContinuation_SurvivesRoundTrip(t *testing.T) {
	c := suspension.Continuation{
		Messages:     []llm.Message{{Role: "user", Content: "hello"}},
		SystemPrompt: "be brief",
		StepIndex:    3,
		TokensUsed:   120,
	}
	blob, err := json.Marshal(&c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back suspension.Continuation
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.StepIndex != 3 || back.TokensUsed != 120 || len(back.Messages) != 1 {
		t.Errorf("continuation lost fidelity: %+v", back)
	}
}

// A malformed continuation must fail at the boundary rather than
// producing a half-restored run. The host this design came from stored
// its continuation in an untyped metadata map and tolerated a bad
// restore in silence; typed columns are the point.
func TestContinuation_RejectsMalformed(t *testing.T) {
	var c suspension.Continuation
	if err := json.Unmarshal([]byte(`{"step_index":"not-a-number"}`), &c); err == nil {
		t.Error("a malformed continuation unmarshalled without error")
	}
}

func TestSuspensionID_HasItsOwnPrefix(t *testing.T) {
	sid := id.NewSuspensionID()
	if _, err := id.ParseSuspensionID(sid.String()); err != nil {
		t.Errorf("a freshly minted suspension id must parse as one: %v", err)
	}
	// A session id must NOT parse as a suspension id, or the prefix is
	// doing nothing and a caller could pass the wrong entity's id anywhere.
	if _, err := id.ParseSuspensionID(id.NewSessionID().String()); err == nil {
		t.Error("a session id parsed as a suspension id; the prefix is not discriminating")
	}
}

func TestSuspendReason_Values(t *testing.T) {
	if suspension.ReasonApproval == suspension.ReasonExternalTool {
		t.Fatal("ReasonApproval and ReasonExternalTool must be distinct values")
	}
}

var _ suspension.Store = (*fakeStore)(nil)

// fakeStore exists only to pin the Store interface's method set at
// compile time, so a shape change here breaks this package's build
// instead of silently landing on whichever store package implements it
// first.
type fakeStore struct{}

func (fakeStore) CreateSuspension(_ context.Context, _ *suspension.Suspension) error {
	return nil
}

func (fakeStore) GetSuspension(_ context.Context, _ id.AgentRunID) (*suspension.Suspension, error) {
	return nil, nil
}

func (fakeStore) ClaimSuspension(_ context.Context, _ id.AgentRunID) (*suspension.Suspension, error) {
	return nil, nil
}

func (fakeStore) DeleteSuspension(_ context.Context, _ id.AgentRunID) error {
	return nil
}

func (fakeStore) ListExpired(_ context.Context, _ time.Time, _ int) ([]*suspension.Suspension, error) {
	return nil, nil
}
