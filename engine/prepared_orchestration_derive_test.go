package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/orchestration"
)

func TestPreparedOrchestrationDerivePreservesFrozenTeam(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	original, err := f.p.Inspect()
	require.NoError(t, err)
	child, err := original.Participants["a"].Inspect()
	require.NoError(t, err)
	var section string
	for _, part := range child.Sections {
		if !part.Locked {
			section = part.ID
			break
		}
	}
	require.NotEmpty(t, section)
	live, err := f.s.GetByName(f.ctx, "a")
	require.NoError(t, err)
	live.Model = "different-live-model"
	require.NoError(t, f.s.Update(f.ctx, live))
	edits := map[string]engine.PreparedEdits{"a": {SectionBodies: map[string]string{section: "review carefully"}}}
	derived, err := f.p.Derive(edits)
	require.NoError(t, err)
	require.NotEqual(t, f.p.Digest(), derived.Digest())
	view, err := derived.Inspect()
	require.NoError(t, err)
	require.Equal(t, original.Config, view.Config)
	require.Equal(t, original.Snapshot, view.Snapshot)
	require.Equal(t, original.Participants["b"].Digest(), view.Participants["b"].Digest())
	changed, err := view.Participants["a"].Inspect()
	require.NoError(t, err)
	require.Equal(t, child.Config, changed.Config)
	require.Equal(t, child.Tools, changed.Tools)
	require.Equal(t, original.Participants["a"].Digest(), changed.Provenance.DerivedFrom)
	edits["a"].SectionBodies[section] = "later mutation"
	encoded, err := json.Marshal(derived)
	require.NoError(t, err)
	var restored engine.PreparedOrchestration
	require.NoError(t, json.Unmarshal(encoded, &restored))
	require.Equal(t, derived.Digest(), restored.Digest())
	baseline, err := f.p.Inspect()
	require.NoError(t, err)
	require.Equal(t, original.Participants["a"].Digest(), baseline.Participants["a"].Digest())
}

func TestPreparedOrchestrationDeriveRejectsExpansion(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	expanded := []string{"invented"}
	for _, edits := range []map[string]engine.PreparedEdits{
		{"invented": {}},
		{"a": {Tools: &expanded}},
		{"a": {SectionBodies: map[string]string{"absent": "new"}}},
	} {
		derived, err := f.p.Derive(edits)
		require.ErrorIs(t, err, cortex.ErrInvalidPrepared)
		require.Nil(t, derived)
	}
	same, err := f.p.Derive(nil)
	require.NoError(t, err)
	require.Equal(t, f.p.Digest(), same.Digest())
	var absent *engine.PreparedOrchestration
	_, err = absent.Derive(nil)
	require.ErrorIs(t, err, cortex.ErrInvalidPrepared)
}
