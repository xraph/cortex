package workerstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/acceptance"
	"github.com/xraph/cortex/sandbox"
)

// Finalizing holds a proposed result while independent cleanup remains pending.
const Finalizing Phase = "finalizing"

// Proposal binds a report to its original claim. Terminal records retain the
// digest and receipt time, while their Result field owns any collectible bytes.
type Proposal struct {
	ClaimRevision uint64          `json:"claim_revision"`
	ReceivedAt    time.Time       `json:"received_at"`
	Digest        string          `json:"digest"`
	Discarded     bool            `json:"discarded,omitempty"`
	Result        *sandbox.Result `json:"result,omitempty"`
}

// Propose fences the claim and persists a report for independent finalization.
// The daemon must authenticate the worker and recheck grants before this call.
// Identical retries return current state without granting execution authority.
func (s *Store) Propose(ctx context.Context, key id.ID, token string, expected uint64, result sandbox.Result) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.load(ctx, key)
	if err != nil {
		return Record{}, err
	}
	if !matchesToken(record, token) || record.CancelRequested {
		return Record{}, ErrConflict
	}
	if resultErr := terminalResult(result, record.Receipt.Job); resultErr != nil {
		return Record{}, resultErr
	}
	if captureErr := s.confirmedCaptures(ctx, record, result); captureErr != nil {
		return Record{}, captureErr
	}
	digest, err := resultDigest(result)
	if err != nil {
		return Record{}, err
	}
	if record.Proposal != nil {
		if record.Proposal.Discarded || (record.Phase != Finalizing && record.Phase != Terminal) || record.Proposal.ClaimRevision != expected || record.Proposal.Digest != digest {
			return Record{}, ErrConflict
		}
		if record.Phase == Terminal {
			committed, hashErr := resultDigest(*record.Result)
			if hashErr != nil || committed != digest {
				return Record{}, ErrConflict
			}
		}
		return record, nil
	}
	if record.Phase != Claimed || record.Revision != expected {
		return Record{}, ErrConflict
	}
	operation, finish, admissionErr := s.reportContext(ctx, record.Receipt)
	if admissionErr != nil {
		return Record{}, admissionErr
	}
	defer finish()
	record.Phase = Finalizing
	record.Proposal = &Proposal{ClaimRevision: expected, ReceivedAt: s.cfg.Now().UTC(), Digest: digest, Result: &result}
	if saveErr := s.save(operation, &record); saveErr != nil {
		return Record{}, saveErr
	}
	return s.load(operation, key)
}

// Finalized commits a report at the exact revision whose whole-service cleanup
// was independently proved by the caller. Recheck grants before calling; this
// journal checks the original acceptance and qualification deadlines once more.
// Workers cannot call this method through the private control transport.
func (s *Store) Finalized(ctx context.Context, key id.ID, expected uint64) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.load(ctx, key)
	if err != nil {
		return Record{}, err
	}
	if record.Phase != Finalizing || record.Revision != expected {
		return Record{}, ErrConflict
	}
	if captureErr := s.confirmedCaptures(ctx, record, *record.Proposal.Result); captureErr != nil {
		return Record{}, captureErr
	}
	operation, finish, admissionErr := s.reportContext(ctx, record.Receipt)
	if admissionErr != nil {
		return Record{}, admissionErr
	}
	defer finish()
	record.Phase = Terminal
	record.Result = record.Proposal.Result
	record.Proposal.Result = nil
	if saveErr := s.save(operation, &record); saveErr != nil {
		return Record{}, saveErr
	}
	return s.load(operation, key)
}

func resultDigest(result sandbox.Result) (string, error) {
	body, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return evidence.Digest(body), nil
}

func validateProposal(record Record) error {
	proposal := record.Proposal
	if proposal == nil {
		return nil
	}
	if record.SchemaVersion != RecordVersion || proposal.ClaimRevision < 2 || proposal.ClaimRevision >= record.Revision || proposal.ReceivedAt.IsZero() || proposal.ReceivedAt.Before(record.Receipt.AcceptedAt) || !proposal.ReceivedAt.Before(acceptance.Deadline(record.Receipt)) || !evidence.ValidDigest(proposal.Digest) {
		return ErrCorrupt
	}
	if record.Phase == Terminal {
		if proposal.Result != nil {
			return ErrCorrupt
		}
		if !proposal.Discarded {
			if record.CancelRequested {
				return ErrCorrupt
			}
			digest, err := resultDigest(*record.Result)
			if err != nil || digest != proposal.Digest {
				return ErrCorrupt
			}
		} else if record.Result.State != sandbox.StateFailed && record.Result.State != sandbox.StateCancelled {
			return ErrCorrupt
		}
		return nil
	}
	if (record.Phase != Finalizing && record.Phase != Reaping) || proposal.Result == nil {
		return ErrCorrupt
	}
	if proposal.Discarded != (record.Phase == Reaping) {
		return ErrCorrupt
	}
	if err := terminalResult(*proposal.Result, record.Receipt.Job); err != nil {
		return err
	}
	digest, err := resultDigest(*proposal.Result)
	if err != nil || digest != proposal.Digest {
		return ErrCorrupt
	}
	return nil
}

func (s *Store) reportContext(ctx context.Context, receipt sandbox.Receipt) (context.Context, context.CancelFunc, error) {
	if err := s.admit(receipt); err != nil {
		return nil, nil, err
	}
	operation, finish := context.WithDeadline(ctx, acceptance.Deadline(receipt))
	return operation, finish, nil
}
