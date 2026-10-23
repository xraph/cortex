package coordination_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestSubmissionRequiresBoundEvidence(t *testing.T) {
	h, _, _, now := handoffFixture(t)
	h, err := coordination.SealHandoff(h)
	require.NoError(t, err)
	current, err := coordination.Propose(h, now)
	require.NoError(t, err)
	change := coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: 1, HandoffDigest: h.Digest, Fence: h.Fence, State: coordination.AssignmentAccepted, Actor: h.Receiver.Principal, Reason: "accepted"}
	current, err = coordination.Advance(current, change, now)
	require.NoError(t, err)
	change.ExpectedRevision, change.State = current.Revision, coordination.AssignmentWorking
	current, err = coordination.Advance(current, change, now)
	require.NoError(t, err)
	change.ExpectedRevision, change.State = current.Revision, coordination.AssignmentSubmitted
	_, err = coordination.Advance(current, change, now)
	require.Error(t, err, "a text claim cannot submit work")
	change.Submission = &coordination.Submission{SchemaVersion: 1, JobID: id.New(verification.PrefixJob), JobRequestDigest: evidence.Digest([]byte("job")), RunID: id.NewAgentRunID(), RunRevision: 3, BindingDigest: evidence.Digest([]byte("binding"))}
	submitted, err := coordination.Advance(current, change, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, coordination.AssignmentSubmitted, submitted.State)
	require.NoError(t, submitted.Validate())
	change.Submission.BindingDigest = evidence.Digest([]byte("mutated"))
	require.NoError(t, submitted.Validate(), "submission must be detached")
	cancelled, err := coordination.Advance(submitted, coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: submitted.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: coordination.AssignmentCancelled, Actor: h.Sender.Principal, Reason: "cancelled"}, h.Deadline.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, submitted.Submission, cancelled.Submission, "cancellation must preserve the submitted evidence pins")
}
