//go:build linux || darwin

package service

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerpeer"
	"github.com/xraph/cortex/sandbox"
)

// WorkerReportReceipt acknowledges durable receipt of a report. It establishes
// neither completed cleanup nor a collectible result, and grants no execution.
type WorkerReportReceipt struct {
	SchemaVersion  int    `json:"schema_version"`
	JobID          id.ID  `json:"job_id"`
	ClaimRevision  uint64 `json:"claim_revision"`
	ProposalDigest string `json:"proposal_digest"`
}

// ProposeWorkerResult authenticates a worker report and retains it for cleanup.
// The controller's scanner independently finalizes or discards the report.
func (c *Controller) ProposeWorkerResult(ctx context.Context, peer *workerpeer.Peer, claim WorkerClaim, result sandbox.Result) (WorkerReportReceipt, error) {
	if peer == nil {
		return WorkerReportReceipt{}, broker.ErrDenied
	}
	return c.proposeWorkerResult(ctx, peer, claim, result)
}

func (c *Controller) proposeWorkerResult(ctx context.Context, peer workerIdentity, claim WorkerClaim, result sandbox.Result) (WorkerReportReceipt, error) {
	// Keep this operation registered through the journal write, after the nested
	// authorization call releases its own controller lifecycle registration.
	operation, finish, err := c.begin(ctx)
	if err != nil {
		return WorkerReportReceipt{}, err
	}
	defer finish()
	operation, bound := context.WithTimeout(operation, 30*time.Second)
	defer bound()
	current, err := c.authorizeWorker(operation, peer, claim)
	if err != nil {
		return WorkerReportReceipt{}, err
	}
	operation, cancel := context.WithDeadline(operation, acceptanceDeadline(current.Receipt))
	defer cancel()
	proposed, err := c.launcher.store.Propose(operation, claim.JobID, claim.Token, claim.Revision, result)
	if err != nil {
		return WorkerReportReceipt{}, err
	}
	return WorkerReportReceipt{SchemaVersion: 1, JobID: claim.JobID, ClaimRevision: proposed.Proposal.ClaimRevision, ProposalDigest: proposed.Proposal.Digest}, nil
}
