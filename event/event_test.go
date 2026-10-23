package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestPublicEventContainsOnlyTypedMetadata(t *testing.T) {
	e := event.Event{SchemaVersion: 1, Input: event.Input{ID: id.New(verification.PrefixCommitEvent), Type: event.ResultCommitted, ResultID: id.New(verification.PrefixResult)}, RunID: id.NewAgentRunID(), Sequence: 1, RunRevision: 2, ObservedAt: time.Now().UTC()}
	require.NoError(t, e.Validate())
	for _, mutate := range []func(*event.Event){
		func(e *event.Event) { e.Type = "private candidate" },
		func(e *event.Event) { e.Sequence = 0 },
		func(e *event.Event) { e.ResultID = id.NewAgentID() },
		func(e *event.Event) { e.SchemaVersion = 2 },
		func(e *event.Event) { e.ID = id.New(event.PrefixEvent) },
	} {
		changed := e
		mutate(&changed)
		require.Error(t, changed.Validate())
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	require.NotContains(t, string(data), "candidate")
}

func TestPublicCursorBindsRunAndSchema(t *testing.T) {
	c := event.Cursor{SchemaVersion: 1, RunID: id.NewAgentRunID()}
	require.NoError(t, (event.ReadRequest{After: c, Limit: 10}).Validate())
	c.SchemaVersion = 2
	require.Error(t, (event.ReadRequest{After: c, Limit: 10}).Validate())
	c.SchemaVersion = 1
	require.Error(t, (event.ReadRequest{After: c, Limit: event.MaxPageSize + 1}).Validate())
}

func TestVerificationPassedEventRequiresCommittedResultIdentity(t *testing.T) {
	e := event.Event{SchemaVersion: 1, Input: event.Input{ID: id.New(event.PrefixEvent), Type: event.VerificationPassed, ResultID: id.New(verification.PrefixResult)}, RunID: id.NewAgentRunID(), Sequence: 1, RunRevision: 2, ObservedAt: time.Now().UTC()}
	require.NoError(t, e.Validate())
	e.ResultID = id.ID{}
	require.Error(t, e.Validate())
	e.ResultID = id.New(verification.PrefixResult)
	e.Type = event.VerificationStale
	require.Error(t, e.Validate())
	e.ResultID = id.ID{}
	require.NoError(t, e.Validate())
}
