//go:build linux || darwin

package service

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerpeer"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// WorkerClaim carries a private credential reference. Never log its token.
type WorkerClaim struct {
	SchemaVersion int    `json:"schema_version"`
	JobID         id.ID  `json:"job_id"`
	Token         string `json:"token"`
	Revision      uint64 `json:"revision"`
	ProfileDigest string `json:"profile_digest"`
}

type workerIdentity interface {
	Check(context.Context, id.ID) (uint32, error)
}

func validateWorkerClaim(claim WorkerClaim) error {
	if claim.SchemaVersion != 1 || claim.JobID.IsNil() || claim.JobID.Prefix() != sandbox.PrefixJob || len(claim.Token) != 64 || claim.Revision < 2 || !evidence.ValidDigest(claim.ProfileDigest) {
		return broker.ErrDenied
	}
	return nil
}

// AuthorizeWorker verifies a live local peer, effective service configuration,
// current grants and durable ownership. It issues no new claim and persists no
// completion. The worker must repeat this check before guest execution after
// staging; successful authorization does not extend its acceptance deadline.
// The caller owns peer and must close it when its socket connection ends.
func (c *Controller) AuthorizeWorker(ctx context.Context, peer *workerpeer.Peer, claim WorkerClaim) (workerstore.Record, error) {
	if peer == nil {
		return workerstore.Record{}, broker.ErrDenied
	}
	return c.authorizeWorker(ctx, peer, claim)
}

func (c *Controller) authorizeWorker(ctx context.Context, peer workerIdentity, claim WorkerClaim) (workerstore.Record, error) {
	operation, finish, err := c.begin(ctx)
	if err != nil {
		return workerstore.Record{}, err
	}
	defer finish()
	operation, bound := context.WithTimeout(operation, 30*time.Second)
	defer bound()
	if claimErr := validateWorkerClaim(claim); claimErr != nil {
		return workerstore.Record{}, claimErr
	}
	digest, err := c.launcher.profile.Digest()
	if err != nil {
		return workerstore.Record{}, err
	}
	if digest != claim.ProfileDigest {
		return workerstore.Record{}, broker.ErrDenied
	}
	pid, err := peer.Check(operation, claim.JobID)
	if err != nil {
		return workerstore.Record{}, err
	}
	record, err := c.launcher.store.VerifyClaim(operation, claim.JobID, claim.Token, claim.Revision)
	if err != nil {
		return workerstore.Record{}, err
	}
	operation, cancel := context.WithDeadline(operation, acceptanceDeadline(record.Receipt))
	defer cancel()
	if checkErr := c.scanner.eligible(operation, record.Receipt); checkErr != nil {
		return workerstore.Record{}, checkErr
	}
	if c.checkWorker == nil {
		return workerstore.Record{}, fmt.Errorf("cortex: missing running worker verifier")
	}
	workerErr := c.checkWorker(operation, c.launcher.profile, claim.JobID, pid)
	if workerErr != nil && workerErr != errWorkerActivating { //nolint:errorlint // Only the sole activation signal may continue to the final authority checks.
		return workerstore.Record{}, workerErr
	}
	if checkErr := c.scanner.eligible(operation, record.Receipt); checkErr != nil {
		return workerstore.Record{}, checkErr
	}
	current, err := peer.Check(operation, claim.JobID)
	if err != nil {
		return workerstore.Record{}, err
	}
	if current != pid {
		return workerstore.Record{}, broker.ErrDenied
	}
	verified, err := c.launcher.store.VerifyClaim(operation, claim.JobID, claim.Token, claim.Revision)
	if err != nil {
		return workerstore.Record{}, err
	}
	if workerErr != nil {
		return workerstore.Record{}, workerErr
	}
	return verified, nil
}

func (c *Controller) beginWorkerOperation(ctx context.Context, peer workerIdentity, claim WorkerClaim) (context.Context, func(), error) {
	operation, finish, err := c.begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	operation, bound := context.WithTimeout(operation, 30*time.Second)
	current, err := c.authorizeWorker(operation, peer, claim)
	if err != nil {
		bound()
		finish()
		return nil, nil, err
	}
	operation, deadline := context.WithDeadline(operation, acceptanceDeadline(current.Receipt))
	return operation, func() { deadline(); bound(); finish() }, nil
}
