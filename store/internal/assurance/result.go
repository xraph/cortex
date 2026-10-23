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

// RunTransaction fences the run and all evidence reads until commit.
// Implementations must roll back both the result record and run update on error.
type RunTransaction interface {
	Driver
	GetRun(context.Context, id.AgentRunID) (*run.Run, error)
	SetRunVerification(context.Context, id.AgentRunID, int64, verification.Status, id.ID, time.Time) error
	SetRunCandidate(context.Context, *run.Run, time.Time) error
	ReplaceJob(context.Context, Record, string) error
	ReplaceEventStream(context.Context, Record, string) error
}

// RunDriver supplies a database transaction; callbacks contain no remote I/O.
// Mongo callbacks may repeat after transaction conflicts and receive session context.
type RunDriver interface {
	TransactRun(context.Context, id.AgentRunID, func(context.Context, RunTransaction) error) error
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
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return verification.CommitReceipt{}, verification.ErrCommitUnavailable
	}
	var receipt verification.CommitReceipt
	err = driver.TransactRun(ctx, frozen.Current.RunID, func(txctx context.Context, tx RunTransaction) error {
		receipt = verification.CommitReceipt{}
		repository := s.inTransaction(tx)
		existing, readErr := repository.GetCommittedResult(txctx, frozen.ResultID)
		if readErr == nil {
			if existing.RequestDigest != digest {
				return verification.ErrConflict
			}
			receipt = existing
			return nil
		}
		if !errors.Is(readErr, verification.ErrNotFound) {
			return readErr
		}
		var commitErr error
		receipt, commitErr = repository.commitResult(txctx, tx, frozen, digest)
		return commitErr
	})
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	return receipt, nil
}

func (s *Repository) commitResult(ctx context.Context, tx RunTransaction, req verification.CommitRequest, digest string) (verification.CommitReceipt, error) {
	if err := s.validateIntentCommit(ctx, req); err != nil {
		return verification.CommitReceipt{}, err
	}
	r, err := tx.GetRun(ctx, req.Current.RunID)
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	if checkErr := s.validateCapturedRun(ctx, r, req.ExpectedRunRevision, req.ContractID, req.Current); checkErr != nil {
		return verification.CommitReceipt{}, checkErr
	}
	if r.VerificationStatus != verification.StatusPending {
		return verification.CommitReceipt{}, cortex.ErrRunConflict
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
	var candidateOutput string
	for index, ref := range append([]evidence.Ref{req.Current.Candidate}, req.Current.Evidence...) {
		artifact, readErr := s.LoadEvidence(ctx, ref)
		if readErr != nil {
			return verification.CommitReceipt{}, readErr
		}
		total += len(artifact.Data)
		if total > 8<<20 {
			return verification.CommitReceipt{}, verification.ErrInvalid
		}
		if index == 0 {
			candidateOutput = string(artifact.Data)
		}
	}
	observed := s.clock.Now().UTC()
	job, jobRow, jobErr := s.jobForCommit(ctx, req, observed)
	if jobErr != nil {
		return verification.CommitReceipt{}, jobErr
	}
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
	if commitErr := s.publishCandidateResult(ctx, tx, r, req.ResultID, candidateOutput, observed); commitErr != nil {
		return verification.CommitReceipt{}, commitErr
	}
	if jobErr := commitJob(ctx, tx, job, jobRow, req.ResultID, observed); jobErr != nil {
		return verification.CommitReceipt{}, jobErr
	}
	if eventErr := s.appendCommittedEvent(ctx, tx, receipt); eventErr != nil {
		return verification.CommitReceipt{}, eventErr
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
