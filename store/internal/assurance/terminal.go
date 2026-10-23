package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const kindTerminalUpdate = "terminal_update"

type terminalTransaction interface {
	RunTransaction
	UpdateRunSnapshot(context.Context, *run.Run, time.Time) error
}

// CommitTerminalRun commits an unchecked terminal snapshot and metadata together.
func (s *Repository) CommitTerminalRun(ctx context.Context, request event.TerminalUpdate) (event.TerminalReceipt, error) {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return event.TerminalReceipt{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return event.TerminalReceipt{}, err
	}
	if scopeErr := require(ctx, req.Run.Scope); scopeErr != nil {
		return event.TerminalReceipt{}, scopeErr
	}
	var receipt event.TerminalReceipt
	err = s.eventTransaction(ctx, req.Run.ID, func(txctx context.Context, repo *Repository, tx RunTransaction, current *run.Run) error {
		receipt = event.TerminalReceipt{}
		row, readErr := repo.get(txctx, Query{Kind: kindTerminalUpdate, ID: req.EventID.String()})
		if readErr == nil {
			if row.Deleted || row.RunID != req.Run.ID.String() || row.ID != req.EventID.String() || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &receipt) != nil || receipt.Validate(req) != nil {
				return event.ErrConflict
			}
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		if current.Revision != req.Run.Revision || current.AgentID != req.Run.AgentID || current.SessionID != req.Run.SessionID || current.Provenance.VerificationRequired != req.Run.Provenance.VerificationRequired || current.VerificationStatus.Normalized() != verification.StatusUnchecked || current.ResultRef != "" {
			return cortex.ErrRunConflict
		}
		switch current.State {
		case run.StateCreated, run.StateRunning, run.StatePaused:
		default:
			return cortex.ErrRunConflict
		}
		writer, ok := tx.(terminalTransaction)
		if !ok {
			return event.ErrUnavailable
		}
		next := req.Run
		at := s.clock.Now().UTC()
		if at.IsZero() {
			return event.ErrInvalid
		}
		if writeErr := writer.UpdateRunSnapshot(txctx, &next, at); writeErr != nil {
			return writeErr
		}
		if eventErr := repo.appendRunEvents(txctx, tx, next.ID, event.Input{ID: req.EventID, Type: event.ExecutionFinished}); eventErr != nil {
			return eventErr
		}
		receipt = event.TerminalReceipt{SchemaVersion: 1, EventID: req.EventID, RunID: next.ID, RequestDigest: digest, RunRevision: next.Revision, UpdatedAt: next.UpdatedAt}
		saved, recordErr := record(kindTerminalUpdate, req.EventID.String(), "", next.ID.String(), next.Scope, receipt)
		if recordErr != nil {
			return recordErr
		}
		saved.Digest = evidence.Digest([]byte(saved.Body))
		return repo.driver.Insert(txctx, saved)
	})
	if err != nil {
		return event.TerminalReceipt{}, err
	}
	return receipt, nil
}
