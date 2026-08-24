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
	sid := id.NewSessionID()
	c := suspension.Continuation{
		Messages: []llm.Message{
			{Role: "user", Content: "from an earlier run"},
			{Role: "user", Content: "hello"},
		},
		SystemPrompt:    "be brief",
		StepIndex:       3,
		TokensUsed:      120,
		NewMessagesFrom: 1,
		SessionID:       sid,
	}
	blob, err := json.Marshal(&c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back suspension.Continuation
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.SystemPrompt != "be brief" {
		t.Errorf("SystemPrompt did not survive the round trip: %q", back.SystemPrompt)
	}
	if back.StepIndex != 3 || back.TokensUsed != 120 || len(back.Messages) != 2 {
		t.Errorf("continuation lost fidelity: %+v", back)
	}
	// The boundary and the session are what a resume saves by. A
	// continuation that loses either re-saves the history it loaded, or
	// saves into the wrong session.
	if back.NewMessagesFrom != 1 {
		t.Errorf("NewMessagesFrom did not survive the round trip: got %d, want 1", back.NewMessagesFrom)
	}
	if back.SessionID != sid {
		t.Errorf("SessionID did not survive the round trip: got %q, want %q", back.SessionID, sid)
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

func (fakeStore) ListExpiredAcrossScopes(_ context.Context, _ time.Time, _ int) ([]*suspension.Suspension, error) {
	return nil, nil
}

func (fakeStore) ClaimExpiredSuspension(_ context.Context, _ id.AgentRunID, _ time.Time) (*suspension.Suspension, error) {
	return nil, nil
}

// A pending call has to carry enough for the caller to execute it. If
// Arguments is dropped on the way through storage, the external-tool
// path still compiles and still round-trips, and the caller is simply
// handed a tool name with no inputs.
func TestPendingCall_CarriesArguments(t *testing.T) {
	p := suspension.PendingCall{ID: "call_1", Name: "fetch", Arguments: `{"url":"https://x"}`}
	blob, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back suspension.PendingCall
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Arguments != p.Arguments {
		t.Errorf("Arguments did not survive the round trip: got %q, want %q", back.Arguments, p.Arguments)
	}
}
