package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// KindEventStream identifies the bounded mutable replay window for one run.
const KindEventStream = "public_event_stream"
const kindEventIdentity = "public_event_identity"

type publicStream struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         id.AgentRunID `json:"run_id"`
	First         uint64        `json:"first"`
	Last          uint64        `json:"last"`
	Events        []event.Event `json:"events"`
}

func (s *Repository) loadPublicStream(ctx context.Context, runID id.AgentRunID) (publicStream, Record, error) {
	row, err := s.get(ctx, Query{Kind: KindEventStream, ID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return publicStream{SchemaVersion: 1, RunID: runID, First: 1}, Record{}, nil
	}
	if err != nil {
		return publicStream{}, Record{}, err
	}
	var stream publicStream
	if len(row.Body) > 1<<20 || json.Unmarshal([]byte(row.Body), &stream) != nil || row.Deleted || row.RunID != runID.String() || row.ID != runID.String() || row.Digest != evidence.Digest([]byte(row.Body)) || stream.SchemaVersion != 1 || stream.RunID != runID || stream.Last > math.MaxInt64 || stream.First == 0 || stream.First > stream.Last+1 || uint64(len(stream.Events)) != stream.Last+1-stream.First || len(stream.Events) > event.MaxRetainedEvents {
		return publicStream{}, Record{}, event.ErrInvalid
	}
	seen := make(map[id.ID]bool, len(stream.Events))
	for i, item := range stream.Events {
		if item.Validate() != nil || item.RunID != runID || item.Sequence != stream.First+uint64(i) || seen[item.ID] {
			return publicStream{}, Record{}, event.ErrInvalid
		}
		seen[item.ID] = true
	}
	return stream, row, nil
}

func (s *Repository) savePublicStream(ctx context.Context, tx RunTransaction, stream publicStream, previous Record) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	row, err := record(KindEventStream, stream.RunID.String(), "", stream.RunID.String(), scope, stream)
	if err != nil {
		return err
	}
	row.Digest = evidence.Digest([]byte(row.Body))
	if previous.ID == "" {
		return s.driver.Insert(ctx, row)
	}
	return tx.ReplaceEventStream(ctx, row, previous.Body)
}

func (s *Repository) eventTransaction(ctx context.Context, runID id.AgentRunID, callback func(context.Context, *Repository, RunTransaction, *run.Run) error) error {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return event.ErrInvalid
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return event.ErrUnavailable
	}
	return driver.TransactRun(ctx, runID, func(txctx context.Context, tx RunTransaction) error {
		current, err := tx.GetRun(txctx, runID)
		if err != nil {
			return err
		}
		return callback(txctx, s.inTransaction(tx), tx, current)
	})
}

// AppendPublicEvents appends a checked batch under the same run fence as results.
func (s *Repository) AppendPublicEvents(ctx context.Context, request event.AppendRequest) (event.AppendReceipt, error) {
	frozen, digest, err := request.Freeze()
	if err != nil {
		return event.AppendReceipt{}, err
	}
	var receipt event.AppendReceipt
	err = s.eventTransaction(ctx, frozen.RunID, func(txctx context.Context, repository *Repository, tx RunTransaction, current *run.Run) error {
		var appendErr error
		receipt, appendErr = repository.appendPublicEvents(txctx, tx, current, frozen, digest)
		return appendErr
	})
	if err != nil {
		return event.AppendReceipt{}, err
	}
	return receipt, nil
}

func (s *Repository) appendPublicEvents(ctx context.Context, tx RunTransaction, current *run.Run, req event.AppendRequest, digest string) (event.AppendReceipt, error) {
	receipt := event.AppendReceipt{FirstSequence: req.AfterSequence + 1, LastSequence: req.AfterSequence + uint64(len(req.Events))}
	body, err := json.Marshal(receipt)
	if err != nil {
		return event.AppendReceipt{}, err
	}
	found := 0
	for _, input := range req.Events {
		row, readErr := s.get(ctx, Query{Kind: kindEventIdentity, ID: input.ID.String()})
		if errors.Is(readErr, ErrAbsent) {
			continue
		}
		if readErr != nil {
			return event.AppendReceipt{}, readErr
		}
		if row.Deleted || row.ID != input.ID.String() || row.RunID != req.RunID.String() || row.Digest != digest || row.Body != string(body) {
			return event.AppendReceipt{}, event.ErrConflict
		}
		found++
	}
	if found == len(req.Events) {
		return receipt, nil
	}
	if found != 0 {
		return event.AppendReceipt{}, event.ErrConflict
	}
	if current.Revision != req.ExpectedRunRevision {
		return event.AppendReceipt{}, cortex.ErrRunConflict
	}
	stream, previous, err := s.loadPublicStream(ctx, req.RunID)
	if err != nil {
		return event.AppendReceipt{}, err
	}
	if req.AfterSequence != stream.Last {
		return event.AppendReceipt{}, event.ErrConflict
	}
	now := s.clock.Now().UTC()
	if now.IsZero() {
		return event.AppendReceipt{}, event.ErrInvalid
	}
	for i, input := range req.Events {
		if checkErr := s.checkPublicEvent(ctx, current, input); checkErr != nil {
			return event.AppendReceipt{}, checkErr
		}
		stream.Events = append(stream.Events, event.Event{SchemaVersion: 1, Input: input, RunID: req.RunID, Sequence: receipt.FirstSequence + uint64(i), RunRevision: current.Revision, ObservedAt: now})
		row, recordErr := record(kindEventIdentity, input.ID.String(), digest, req.RunID.String(), current.Scope, receipt)
		if recordErr != nil {
			return event.AppendReceipt{}, recordErr
		}
		if insertErr := s.driver.Insert(ctx, row); insertErr != nil {
			return event.AppendReceipt{}, insertErr
		}
	}
	stream.Last = receipt.LastSequence
	if len(stream.Events) > event.MaxRetainedEvents {
		stream.Events = slices.Clone(stream.Events[len(stream.Events)-event.MaxRetainedEvents:])
	}
	stream.First = stream.Last + 1 - uint64(len(stream.Events))
	if saveErr := s.savePublicStream(ctx, tx, stream, previous); saveErr != nil {
		return event.AppendReceipt{}, saveErr
	}
	return receipt, nil
}

func (s *Repository) checkPublicEvent(ctx context.Context, current *run.Run, input event.Input) error {
	switch input.Type {
	case event.ExecutionFinished:
		if !slices.Contains([]run.State{run.StateCompleted, run.StateIncomplete, run.StateFailed, run.StateCancelled}, current.State) {
			return event.ErrConflict
		}
	case event.VerificationPending:
		if current.VerificationStatus != verification.StatusPending {
			return event.ErrConflict
		}
	case event.VerificationFailed:
		if current.VerificationStatus != verification.StatusFailed {
			return event.ErrConflict
		}
	case event.VerificationInconclusive:
		if current.VerificationStatus != verification.StatusInconclusive {
			return event.ErrConflict
		}
	case event.VerificationStale:
		if current.VerificationStatus != verification.StatusStale {
			return event.ErrConflict
		}
	case event.VerificationPassed, event.ResultCommitted:
		receipt, err := s.GetCommittedResult(ctx, input.ResultID)
		if err != nil {
			return err
		}
		if current.VerificationStatus != verification.StatusPassed || current.ResultRef != input.ResultID.String() || receipt.RunRevision != current.Revision || (input.Type == event.ResultCommitted && receipt.Outbox.EventID != input.ID) || receipt.Outbox.RunID != current.ID {
			return event.ErrConflict
		}
	default:
		return event.ErrInvalid
	}
	return nil
}

func streamWatermark(stream publicStream, current *run.Run) (event.Watermark, error) {
	watermark := event.Watermark{SchemaVersion: 1, RunID: current.ID, FirstSequence: stream.First, LastSequence: stream.Last, RunRevision: current.Revision}
	if current.ResultRef != "" {
		resultID, err := id.ParseWithPrefix(current.ResultRef, verification.PrefixResult)
		if err != nil {
			return event.Watermark{}, event.ErrInvalid
		}
		watermark.ResultID = resultID
	}
	return watermark, nil
}

// ReadPublicEvents returns one consistent retained page. It does not authorize a subscriber.
func (s *Repository) ReadPublicEvents(ctx context.Context, request event.ReadRequest) (event.Page, error) {
	if err := request.Validate(); err != nil {
		return event.Page{}, err
	}
	var page event.Page
	gap := false
	err := s.eventTransaction(ctx, request.After.RunID, func(txctx context.Context, repository *Repository, _ RunTransaction, current *run.Run) error {
		page = event.Page{Next: request.After}
		gap = false
		stream, _, readErr := repository.loadPublicStream(txctx, current.ID)
		if readErr != nil {
			return readErr
		}
		page.Watermark, readErr = streamWatermark(stream, current)
		if readErr != nil {
			return readErr
		}
		if request.After.Sequence > stream.Last {
			return event.ErrInvalid
		}
		if request.After.Sequence+1 < stream.First {
			gap = true
			page.Next.Sequence = stream.First - 1
			return nil
		}
		for _, item := range stream.Events {
			if item.Sequence <= request.After.Sequence {
				continue
			}
			page.Events = append(page.Events, item)
			page.Next.Sequence = item.Sequence
			if len(page.Events) == request.Limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return event.Page{}, err
	}
	if gap {
		return page, event.ErrCursorGap
	}
	return page, nil
}

// ReadEventWatermark identifies the current run snapshot and retained sequence range.
func (s *Repository) ReadEventWatermark(ctx context.Context, runID id.AgentRunID) (event.Watermark, error) {
	var watermark event.Watermark
	err := s.eventTransaction(ctx, runID, func(txctx context.Context, repository *Repository, _ RunTransaction, current *run.Run) error {
		stream, _, readErr := repository.loadPublicStream(txctx, runID)
		if readErr != nil {
			return readErr
		}
		watermark, readErr = streamWatermark(stream, current)
		return readErr
	})
	if err != nil {
		return event.Watermark{}, err
	}
	return watermark, nil
}

// PrunePublicEvents drops retained envelopes through a cursor. ID/digest tombstones
// remain so a delayed retry cannot recreate a pruned event or reuse its identity.
func (s *Repository) PrunePublicEvents(ctx context.Context, through event.Cursor) error {
	if err := through.Validate(); err != nil {
		return err
	}
	return s.eventTransaction(ctx, through.RunID, func(txctx context.Context, repository *Repository, tx RunTransaction, _ *run.Run) error {
		stream, previous, err := repository.loadPublicStream(txctx, through.RunID)
		if err != nil {
			return err
		}
		if through.Sequence > stream.Last {
			return event.ErrInvalid
		}
		if through.Sequence < stream.First {
			return nil
		}
		stream.Events = slices.Clone(stream.Events[through.Sequence-stream.First+1:])
		stream.First = through.Sequence + 1
		return repository.savePublicStream(txctx, tx, stream, previous)
	})
}

func (s *Repository) appendCommittedEvent(ctx context.Context, tx RunTransaction, receipt verification.CommitReceipt) error {
	return s.appendRunEvents(ctx, tx, receipt.Outbox.RunID,
		event.Input{ID: id.New(event.PrefixEvent), Type: event.VerificationPassed, ResultID: receipt.Request.ResultID},
		event.Input{ID: receipt.Outbox.EventID, Type: event.ResultCommitted, ResultID: receipt.Request.ResultID})
}

func (s *Repository) appendRunEvents(ctx context.Context, tx RunTransaction, runID id.AgentRunID, inputs ...event.Input) error {
	current, err := tx.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	stream, _, err := s.loadPublicStream(ctx, current.ID)
	if err != nil {
		return err
	}
	request, digest, err := (event.AppendRequest{SchemaVersion: 1, RunID: current.ID, ExpectedRunRevision: current.Revision, AfterSequence: stream.Last, Events: inputs}).Freeze()
	if err != nil {
		return err
	}
	_, err = s.appendPublicEvents(ctx, tx, current, request, digest)
	return err
}

// setVerificationWithEvent keeps the current run verdict and its observation in
// one transaction. Passed events belong exclusively to the guarded result commit.
func (s *Repository) setVerificationWithEvent(ctx context.Context, tx RunTransaction, runID id.AgentRunID, revision int64, status verification.Status, at time.Time) error {
	var kind event.Type
	switch status {
	case verification.StatusPending:
		kind = event.VerificationPending
	case verification.StatusFailed:
		kind = event.VerificationFailed
	case verification.StatusInconclusive:
		kind = event.VerificationInconclusive
	case verification.StatusStale:
		kind = event.VerificationStale
	default:
		return event.ErrInvalid
	}
	if err := tx.SetRunVerification(ctx, runID, revision, status, id.ID{}, at); err != nil {
		return err
	}
	return s.appendRunEvents(ctx, tx, runID, event.Input{ID: id.New(event.PrefixEvent), Type: kind})
}
