package coordination_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
)

func TestAssignmentLifecycle(t *testing.T) {
	h, _, _, now := handoffFixture(t)
	h, err := coordination.SealHandoff(h)
	require.NoError(t, err)
	initial, err := coordination.Propose(h, now)
	require.NoError(t, err)
	require.Equal(t, coordination.AssignmentProposed, initial.State)
	previous := initial
	for i, state := range []coordination.AssignmentState{coordination.AssignmentAccepted, coordination.AssignmentWorking, coordination.AssignmentBlocked, coordination.AssignmentWorking, coordination.AssignmentCancelled} {
		change := coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: previous.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: state, Actor: h.Receiver.Principal, Reason: "observed by host"}
		next, advanceErr := coordination.Advance(previous, change, now.Add(time.Duration(i+1)*time.Second))
		require.NoError(t, advanceErr)
		require.Equal(t, previous.Revision+1, next.Revision)
		require.Equal(t, previous.Digest, next.PreviousDigest)
		require.Equal(t, h, next.Handoff)
		require.NoError(t, next.Validate())
		previous = next
	}
	_, err = coordination.Advance(previous, coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: previous.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: coordination.AssignmentWorking, Actor: h.Receiver.Principal, Reason: "restart"}, now.Add(time.Minute))
	require.ErrorIs(t, err, coordination.ErrAssignmentConflict)
	require.NoError(t, initial.Validate())
	previous.Handoff.Criteria[0] = "mutated"
	require.NoError(t, initial.Validate(), "snapshots must be detached")
}

func TestAssignmentRejectsUnboundTransitions(t *testing.T) {
	for _, mode := range []string{"skip", "actor", "revision", "digest", "fence", "time", "expired", "success", "submitted", "reviewed", "reason"} {
		t.Run(mode, func(t *testing.T) {
			h, _, _, now := handoffFixture(t)
			h, err := coordination.SealHandoff(h)
			require.NoError(t, err)
			initial, err := coordination.Propose(h, now)
			require.NoError(t, err)
			change := coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: 1, HandoffDigest: h.Digest, Fence: h.Fence, State: coordination.AssignmentAccepted, Actor: h.Receiver.Principal, Reason: "accepted"}
			at := now.Add(time.Second)
			switch mode {
			case "skip":
				change.State = coordination.AssignmentWorking
			case "actor":
				change.Actor = h.Sender.Principal
			case "revision":
				change.ExpectedRevision++
			case "digest":
				change.HandoffDigest = initial.Digest
			case "fence":
				change.Fence++
			case "time":
				at = now.Add(-time.Second)
			case "expired":
				at = h.Deadline
			case "success":
				change.State = "succeeded"
			case "submitted":
				change.State = "submitted"
			case "reviewed":
				change.State = "reviewed"
			case "reason":
				change.Reason = ""
			}
			_, err = coordination.Advance(initial, change, at)
			require.Error(t, err)
		})
	}
}
