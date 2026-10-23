package engine_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func TestPreparedAssignmentProvenance(t *testing.T) {
	ctx := overlayCtx("assignment")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
	e, s, _ := newConfigEngine(t, ctx, ag)
	prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
	require.NoError(t, err)
	parent := id.NewAgentRunID().String()
	digest := evidence.Digest([]byte("handoff"))
	for _, mode := range []string{"digest", "parent", "valid"} {
		t.Run(mode, func(t *testing.T) {
			provenance := run.ExecutionProvenance{ParentRunID: parent, AssignmentDigest: digest}
			if mode == "digest" {
				provenance.AssignmentDigest = "invalid"
			}
			if mode == "parent" {
				provenance.ParentRunID = ""
			}
			r, runErr := e.RunPrepared(ctx, prepared, "work", engine.PreparedRunOptions{Provenance: provenance})
			if mode != "valid" {
				require.ErrorIs(t, runErr, cortex.ErrInvalidPrepared)
				return
			}
			require.NoError(t, runErr)
			require.Equal(t, digest, r.Provenance.AssignmentDigest)
			saved, readErr := s.GetRun(ctx, r.ID)
			require.NoError(t, readErr)
			require.Equal(t, digest, saved.Provenance.AssignmentDigest)
			require.Equal(t, parent, saved.Provenance.ParentRunID)
		})
	}
}
