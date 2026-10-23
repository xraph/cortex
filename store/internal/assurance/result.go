package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const kindResult = "result"

// ResultTransaction fences the run and all evidence reads until commit.
// Implementations must roll back both the result record and run update on error.
type ResultTransaction interface {
	Driver
	GetRun(context.Context, id.AgentRunID) (*run.Run, error)
	CommitRun(context.Context, id.AgentRunID, int64, id.ID, time.Time) error
}

// ResultDriver supplies a database transaction; callbacks contain no remote I/O.
// Mongo callbacks may repeat after transaction conflicts and receive session context.
type ResultDriver interface {
	TransactResult(context.Context, id.AgentRunID, func(context.Context, ResultTransaction) (verification.CommitReceipt, error)) (verification.CommitReceipt, error)
}

// CommitVerifiedResult atomically saves a checked result, its outbox and run revision.
func (s *Repository) CommitVerifiedResult(ctx context.Context, request verification.CommitRequest) (verification.CommitReceipt, error) {
	if err := require(ctx, request.Current.Scope); err != nil {
		return verification.CommitReceipt{}, err
	}
	frozen, digest, err := request.Freeze()
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	driver, ok := s.driver.(ResultDriver)
	if !ok {
		return verification.CommitReceipt{}, verification.ErrCommitUnavailable
	}
	receipt, err := driver.TransactResult(ctx, frozen.Current.RunID, func(txctx context.Context, tx ResultTransaction) (verification.CommitReceipt, error) {
		repository := New(tx)
		existing, readErr := repository.GetCommittedResult(txctx, frozen.ResultID)
		if readErr == nil {
			if existing.RequestDigest != digest {
				return verification.CommitReceipt{}, verification.ErrConflict
			}
			return existing, nil
		}
		if !errors.Is(readErr, verification.ErrNotFound) {
			return verification.CommitReceipt{}, readErr
		}
		return repository.commitResult(txctx, tx, frozen, digest)
	})
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	return receipt, nil
}

func (s *Repository) commitResult(ctx context.Context, tx ResultTransaction, req verification.CommitRequest, digest string) (verification.CommitReceipt, error) {
	r, err := tx.GetRun(ctx, req.Current.RunID)
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	if r.Scope.Canonical() != req.Current.Scope.Canonical() {
		return verification.CommitReceipt{}, cortex.ErrRunNotFound
	}
	if r.Revision != req.ExpectedRunRevision || r.State != run.StateCompleted || r.StopReason != run.StopFinalAnswer || r.VerificationStatus != verification.StatusPending || r.ResultRef != "" || r.Provenance.PreparedDigest != req.Current.Context.PreparedDigest || r.Provenance.AcceptanceRef != req.ContractID.String() {
		return verification.CommitReceipt{}, cortex.ErrRunConflict
	}
	if int64(len(r.Output)) != req.Current.Candidate.Bytes || evidence.Digest([]byte(r.Output)) != req.Current.Candidate.SHA256 {
		return verification.CommitReceipt{}, verification.ErrConflict
	}
	contract, err := s.GetContract(ctx, req.ContractID)
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	if contract.Digest != req.Current.ContractDigest {
		return verification.CommitReceipt{}, verification.ErrConflict
	}
	reports := make([]verification.Report, 0, len(req.ReportIDs))
	digests := make([]string, 0, len(req.ReportIDs))
	// Bound total material processed while locks are held, in addition to each
	// record's one MiB limit. Artifact bytes are released between loads.
	total := 0
	for _, reportID := range req.ReportIDs {
		report, readErr := s.GetReport(ctx, reportID)
		if readErr != nil {
			return verification.CommitReceipt{}, readErr
		}
		body, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			return verification.CommitReceipt{}, marshalErr
		}
		total += len(body)
		if total > 8<<20 {
			return verification.CommitReceipt{}, verification.ErrInvalid
		}
		reports = append(reports, report)
		digests = append(digests, report.Digest)
	}
	for _, ref := range append([]evidence.Ref{req.Current.Candidate}, req.Current.Evidence...) {
		artifact, readErr := s.LoadEvidence(ctx, ref)
		if readErr != nil {
			return verification.CommitReceipt{}, readErr
		}
		total += len(artifact.Data)
		if total > 8<<20 {
			return verification.CommitReceipt{}, verification.ErrInvalid
		}
	}
	observed := time.Now().UTC()
	for _, report := range reports {
		if verification.CheckFreshness(report, req.Current, observed) != verification.StatusPassed {
			return verification.CommitReceipt{}, verification.ErrConflict
		}
	}
	if verification.Aggregate(contract, reports, observed).Status != verification.StatusPassed {
		return verification.CommitReceipt{}, verification.ErrConflict
	}
	bindingDigest, err := req.Current.Digest()
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	receipt, err := verification.SealCommitReceipt(verification.CommitReceipt{SchemaVersion: 1, Request: req, RequestDigest: digest, RunRevision: r.Revision + 1, ReportDigests: digests, ObservedAt: observed, Outbox: verification.CommitOutbox{EventID: id.New(verification.PrefixCommitEvent), Type: "result_committed", RunID: r.ID, ResultID: req.ResultID, BindingDigest: bindingDigest}})
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	row, err := record(kindResult, req.ResultID.String(), receipt.Digest, r.ID.String(), req.Current.Scope, receipt)
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	if insertErr := s.driver.Insert(ctx, row); insertErr != nil {
		return verification.CommitReceipt{}, insertErr
	}
	if commitErr := tx.CommitRun(ctx, r.ID, r.Revision, req.ResultID, observed); commitErr != nil {
		return verification.CommitReceipt{}, commitErr
	}
	return receipt, nil
}

// GetCommittedResult reads an exact-scope historical receipt, not a fresh verdict.
func (s *Repository) GetCommittedResult(ctx context.Context, resultID id.ID) (verification.CommitReceipt, error) {
	if resultID.IsNil() || resultID.Prefix() != verification.PrefixResult {
		return verification.CommitReceipt{}, verification.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: kindResult, ID: resultID.String()})
	if errors.Is(err, ErrAbsent) {
		return verification.CommitReceipt{}, verification.ErrNotFound
	}
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	if len(row.Body) > 1<<20 {
		return verification.CommitReceipt{}, verification.ErrInvalid
	}
	var receipt verification.CommitReceipt
	if err = json.Unmarshal([]byte(row.Body), &receipt); err != nil {
		return verification.CommitReceipt{}, verification.ErrInvalid
	}
	if validateErr := receipt.Validate(); validateErr != nil {
		return verification.CommitReceipt{}, validateErr
	}
	if row.Deleted || row.ID != receipt.Request.ResultID.String() || row.RunID != receipt.Request.Current.RunID.String() || row.Digest != receipt.Digest || row.ScopeCanon != receipt.Request.Current.Scope.Canonical() {
		return verification.CommitReceipt{}, verification.ErrInvalid
	}
	return receipt, nil
}
