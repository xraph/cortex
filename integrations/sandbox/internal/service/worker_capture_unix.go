//go:build linux || darwin

package service

import (
	"context"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerpeer"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// CaptureWorker authenticates a declared capture and commits its intent or
// readback confirmation. A confirmation records worker testimony, not grading.
func (c *Controller) CaptureWorker(ctx context.Context, peer *workerpeer.Peer, claim WorkerClaim, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
	if peer == nil {
		return WorkerCaptureReceipt{}, broker.ErrDenied
	}
	return c.captureWorker(ctx, peer, claim, ref, confirm)
}

func (c *Controller) captureWorker(ctx context.Context, peer workerIdentity, claim WorkerClaim, ref evidence.Ref, confirm bool) (WorkerCaptureReceipt, error) {
	operation, finish, err := c.begin(ctx)
	if err != nil {
		return WorkerCaptureReceipt{}, err
	}
	defer finish()
	operation, bound := context.WithTimeout(operation, 30*time.Second)
	defer bound()
	if c.capturePolicy == nil {
		return WorkerCaptureReceipt{}, broker.ErrDenied
	}
	header, _, err := prepareCapture(claim, ref, confirm)
	if err != nil {
		return WorkerCaptureReceipt{}, err
	}
	current, err := c.authorizeWorker(operation, peer, claim)
	if err != nil {
		return WorkerCaptureReceipt{}, err
	}
	operation, cancel := context.WithDeadline(operation, acceptanceDeadline(current.Receipt))
	defer cancel()
	job := current.Receipt.Job
	if validationErr := job.ValidateArtifacts([]evidence.Ref{ref}, false); validationErr != nil {
		return WorkerCaptureReceipt{}, validationErr
	}
	candidate, err := sandbox.Seal(job)
	if err != nil {
		return WorkerCaptureReceipt{}, err
	}
	policyRef := ref
	policyRef.Scope.Levels = slices.Clone(ref.Scope.Levels)
	action := evidence.ActionPut
	if confirm {
		action = evidence.ActionRead
	}
	policyContext := cortex.WithScope(operation, cortex.Scope{Levels: slices.Clone(job.Scope.Levels)})
	if policyErr := c.capturePolicy(policyContext, candidate, action, policyRef); policyErr != nil {
		return WorkerCaptureReceipt{}, policyErr
	}
	if _, authorizationErr := c.authorizeWorker(operation, peer, claim); authorizationErr != nil {
		return WorkerCaptureReceipt{}, authorizationErr
	}
	var record workerstore.CaptureRecord
	if confirm {
		record, err = c.launcher.store.ConfirmCapture(operation, claim.JobID, claim.Token, claim.Revision, ref)
	} else {
		record, err = c.launcher.store.RecordCapture(operation, claim.JobID, claim.Token, claim.Revision, ref)
	}
	if err != nil {
		return WorkerCaptureReceipt{}, err
	}
	if err := operation.Err(); err != nil {
		return WorkerCaptureReceipt{}, err
	}
	return WorkerCaptureReceipt{SchemaVersion: 1, JobID: claim.JobID, ClaimRevision: claim.Revision, ArtifactID: ref.ID, BindingDigest: header.Digest, State: record.State, Revision: record.Revision}, nil
}
