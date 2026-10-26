package storetest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

// TaskCompletionFixture supplies real retained producer evidence for runtime
// integration tests. Request identities may be replaced before admission.
type TaskCompletionFixture struct {
	Context            context.Context
	Request            coordination.TaskCompletionRequest
	Acceptance         coordination.TaskAcceptanceRequest
	Review             coordination.Review
	Submission         coordination.Submission
	ProducerAssignment coordination.Assignment
	Plan               planning.Plan
	Contract           verification.Contract
}

// NewTaskCompletionFixture commits a verified producer result. When deferred is
// true, the assignment remains working so the completion service must submit it.
func NewTaskCompletionFixture(t *testing.T, backend store.Store, deferred bool) TaskCompletionFixture {
	t.Helper()
	var f TaskCompletionFixture
	if deferred {
		f.Context, f.Request, f.Acceptance, f.Review = taskCompletionFixture(t, backend, &f.Submission)
	} else {
		f.Context, f.Request, f.Acceptance, f.Review = taskCompletionFixture(t, backend)
	}
	var err error
	f.ProducerAssignment, err = resultCapability[coordination.AssignmentStore](t, backend).GetAssignment(f.Context, f.Request.AssignmentID)
	require.NoError(t, err)
	if !deferred {
		f.Submission = *f.ProducerAssignment.Submission
	}
	f.Plan, err = resultCapability[planning.Store](t, backend).GetPlan(f.Context, f.Request.Handoff.PlanID)
	require.NoError(t, err)
	job, err := resultCapability[verification.JobStore](t, backend).GetVerificationJob(f.Context, f.Submission.JobID)
	require.NoError(t, err)
	f.Contract, err = resultCapability[verification.Store](t, backend).GetContract(f.Context, job.Request.ContractID)
	require.NoError(t, err)
	return f
}
