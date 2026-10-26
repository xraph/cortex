package assurance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

var _ coordination.EventStore = (*Repository)(nil)

// ImportCoordinationEvents materializes existing immutable source intents under
// the plan lock. Replaying a source page cannot change event identity or order.
// History cursors traverse superseded claims without scanning another plan.
func (s *Repository) ImportCoordinationEvents(ctx context.Context, req coordination.EventImport) (coordination.EventImportPage, error) {
	if err := req.Validate(); err != nil {
		return coordination.EventImportPage{}, err
	}
	if err := require(ctx, req.Scope); err != nil {
		return coordination.EventImportPage{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var page coordination.EventImportPage
	err := s.planTransaction(bounded, req.PlanID, func(tc context.Context, tx *Repository) error {
		page = coordination.EventImportPage{}
		if _, readErr := tx.GetPlan(tc, req.PlanID); readErr != nil {
			return readErr
		}
		cursor := req.After
		for range req.Limit {
			events, next, done, readErr := tx.coordinationSourceStep(tc, req, cursor)
			if readErr != nil {
				return readErr
			}
			for _, source := range events {
				if source.PlanID != req.PlanID || source.Scope.Canonical() != req.Scope.Canonical() {
					return planning.ErrConflict
				}
				fresh, writeErr := tx.appendCoordinationEvent(tc, source)
				if writeErr != nil {
					return writeErr
				}
				if fresh {
					page.Imported++
				}
			}
			if done {
				page.Next = ""
				return nil
			}
			cursor = next
		}
		page.Next = cursor
		return tc.Err()
	})
	if err != nil {
		return coordination.EventImportPage{}, err
	}
	return page, nil
}
func (s *Repository) coordinationSourceStep(ctx context.Context, req coordination.EventImport, after string) (projected []coordination.Event, cursorAfter string, finished bool, failure error) {
	if req.Source == "task_claim_history" || req.Source == "effect_continuation_history" {
		return s.coordinationHistoryStep(ctx, req, after)
	}
	rows, err := s.driver.Select(ctx, Query{Kind: req.Source, RunID: req.PlanID.String(), After: after, Limit: 1})
	if err != nil {
		return nil, "", false, err
	}
	if len(rows) == 0 {
		return nil, "", true, nil
	}
	row := rows[0]
	if operationErr := validateRecord(ctx, row); operationErr != nil {
		return nil, "", false, operationErr
	}
	event, err := s.coordinationSourceEvent(ctx, row)
	if err != nil {
		return nil, "", false, err
	}
	return []coordination.Event{event}, row.ID, false, nil
}
func sourceEvent(row Record, plan id.ID) coordination.Event {
	return coordination.Event{SchemaVersion: 1, PlanID: plan, EventID: evidence.Digest([]byte(row.ScopeCanon + "\x00" + plan.String() + "\x00" + row.Kind + "\x00" + row.ID)), Type: row.Kind, SourceKind: row.Kind, SourceID: row.ID, SourceDigest: row.Digest}
}
func claimSourceEvent(row Record, claim coordination.Claim, eventID, kind string) coordination.Event {
	event := sourceEvent(row, claim.PlanID)
	event.Scope = claim.Scope
	event.PlanRevision = claim.PlanRevision
	event.ClaimRevision = claim.Revision
	event.PredecessorDigest = claim.PreviousDigest
	event.TaskID = claim.TaskID
	event.RunID = claim.RunID
	event.ObservedAt = claim.At
	if eventID != "" {
		event.EventID = eventID
	}
	if kind != "" {
		event.Type = kind
	}
	return event
}
func (s *Repository) coordinationSourceEvent(ctx context.Context, row Record) (coordination.Event, error) {
	var event coordination.Event
	switch row.Kind {
	case kindReadyTaskClaim:
		key, err := id.Parse(row.ID)
		if err != nil {
			return event, err
		}
		retained, err := s.getReadyClaim(ctx, key)
		if err != nil {
			return event, err
		}
		event = claimSourceEvent(row, retained.Result.Acquisition.Claim, retained.Result.Outbox.EventID.String(), retained.Result.Outbox.Type)
	case kindReadyTaskDispatch:
		key, err := id.Parse(row.ID)
		if err != nil {
			return event, err
		}
		retained, err := s.getTaskDispatch(ctx, key)
		if err != nil {
			return event, err
		}
		event = claimSourceEvent(row, retained.Result.Claim, retained.Result.Outbox.EventID.String(), retained.Result.Outbox.Type)
	case kindTaskRunStart:
		key, err := id.Parse(row.ID)
		if err != nil {
			return event, err
		}
		retained, err := s.getTaskRunStart(ctx, key)
		if err != nil {
			return event, err
		}
		claim, err := s.claimRevision(ctx, retained.Claim.PlanID, retained.Claim.TaskID, retained.Claim.Revision)
		if err != nil {
			return event, err
		}
		event = claimSourceEvent(row, claim, retained.Result.Outbox.EventID.String(), retained.Result.Outbox.Type)
		event.ObservedAt = retained.Result.CreatedAt
	case kindTaskExecutionRenewal:
		retained, err := s.taskRenewal(ctx, row.ID)
		if err != nil {
			return event, err
		}
		event = claimSourceEvent(row, retained.Claim, retained.Outbox.EventID.String(), retained.Outbox.Type)
	case kindTaskAcceptance:
		retained, err := decodeTaskAcceptance(row)
		if err != nil {
			return event, err
		}
		event = claimSourceEvent(row, retained.Claim, retained.Outbox.EventID.String(), retained.Outbox.Type)
		artifact := retained.Request.Current.Candidate
		event.Artifact = &artifact
	case kindPlanReplan, kindPlanReplanOutcome, kindPlanReplanCommit:
		key, err := id.Parse(row.ID)
		if err != nil {
			return event, err
		}
		admission, err := s.GetPlanReplan(ctx, key)
		if err != nil {
			return event, err
		}
		if admission == nil {
			return event, planning.ErrConflict
		}
		p := admission.Request.Progress.Plan
		event = sourceEvent(row, p.ID)
		event.Scope = p.Scope
		event.PlanRevision = p.Revision
		event.RunID = admission.Request.RunID
		event.ObservedAt = admission.At
		event.PredecessorDigest = p.Digest
		if row.Kind == kindPlanReplanOutcome {
			outcome, readErr := s.GetPlanReplanOutcome(ctx, key)
			if readErr != nil {
				return event, readErr
			}
			if outcome == nil {
				return event, planning.ErrConflict
			}
			event.ObservedAt = outcome.At
			event.PredecessorDigest = admission.RequestDigest
		}
		if row.Kind == kindPlanReplanCommit {
			committed, readErr := s.GetPlanReplanCommit(ctx, key)
			if readErr != nil {
				return event, readErr
			}
			if committed == nil {
				return event, planning.ErrConflict
			}
			event.PlanRevision = committed.Revision
			event.PredecessorDigest = committed.PreviousDigest
			event.ObservedAt = time.Time{}
		}
	case kindArtifactBoardIdentity:
		retained, err := decodeArtifactBoard(ctx, row)
		if err != nil {
			return event, err
		}
		event = boardSourceEvent(retained)
	default:
		return event, planning.ErrInvalid
	}
	if row.Deleted || row.RunID != event.PlanID.String() || row.ScopeCanon != event.Scope.Canonical() {
		return coordination.Event{}, planning.ErrConflict
	}
	return coordination.SealEvent(event)
}
func boardSourceEvent(entry coordination.ArtifactBoardEntry) coordination.Event {
	kind := "artifact_board_published"
	if entry.Conflict {
		kind = "artifact_board_conflict"
	}
	if !entry.Request.Resolve.IsNil() {
		kind = "artifact_board_resolved"
	}
	artifact := entry.Artifact
	return coordination.Event{SchemaVersion: 1, Scope: entry.Request.Scope, PlanID: entry.Request.PlanID, EventID: evidence.Digest([]byte(entry.Request.Scope.Canonical() + "\x00" + entry.Request.PlanID.String() + "\x00" + kindArtifactBoardIdentity + "\x00" + entry.Request.ID.String())), Type: kind, SourceKind: kindArtifactBoardIdentity, SourceID: entry.Request.ID.String(), SourceDigest: entry.Digest, PlanRevision: entry.PlanRevision, PredecessorDigest: entry.PreviousDigest, TaskID: entry.TaskID, RunID: entry.Request.Current.RunID, Artifact: &artifact, ObservedAt: entry.At}
}

type coordinationHistoryCursor struct {
	AfterTask   string `json:"after_task,omitempty"`
	TaskID      id.ID  `json:"task_id,omitempty"`
	Revision    int64  `json:"revision,omitempty"`
	Effects     bool   `json:"effects,omitempty"`
	AfterEffect string `json:"after_effect,omitempty"`
}

func encodeCoordinationCursor(cursor coordinationHistoryCursor) (string, error) {
	data, err := json.Marshal(cursor)
	return string(data), err
}
func (s *Repository) coordinationHistoryStep(ctx context.Context, req coordination.EventImport, after string) (projected []coordination.Event, cursorAfter string, finished bool, failure error) {
	var cursor coordinationHistoryCursor
	if after != "" && json.Unmarshal([]byte(after), &cursor) != nil {
		return nil, "", false, planning.ErrInvalid
	}
	if cursor.Revision < 0 || len(cursor.AfterTask) > 512 || len(cursor.AfterEffect) > 512 || (!cursor.TaskID.IsNil() && cursor.TaskID.Prefix() != planning.PrefixTask) {
		return nil, "", false, planning.ErrInvalid
	}
	events := make([]coordination.Event, 0, 2)
	if cursor.Effects {
		if req.Source != "effect_continuation_history" || cursor.TaskID.IsNil() || cursor.Revision < 1 {
			return nil, "", false, planning.ErrInvalid
		}
		claim, err := s.claimRevision(ctx, req.PlanID, cursor.TaskID, cursor.Revision)
		if err != nil {
			return nil, "", false, err
		}
		if claim.State != coordination.ClaimRecovering || claim.RunID.IsNil() {
			return nil, "", false, planning.ErrConflict
		}
		rows, err := s.driver.Select(ctx, Query{Kind: kindEffectContinuation, RunID: claim.RunID.String(), After: cursor.AfterEffect, Limit: 1})
		if err != nil {
			return nil, "", false, err
		}
		if len(rows) == 0 {
			cursor.Effects = false
			cursor.AfterEffect = ""
		} else {
			row := rows[0]
			projected, readErr := s.continuationSourceEvents(ctx, row, claim)
			if readErr != nil {
				return nil, "", false, readErr
			}
			events = append(events, projected...)
			cursor.AfterEffect = row.ID
		}
	} else {
		query := Query{Kind: kindTaskClaim, RunID: req.PlanID.String(), After: cursor.AfterTask, Limit: 1}
		if !cursor.TaskID.IsNil() {
			query = Query{Kind: kindTaskClaimRevision, RunID: claimKey(req.PlanID, cursor.TaskID), After: claimRevisionKey(req.PlanID, cursor.TaskID, cursor.Revision), Limit: 1}
		}
		rows, err := s.driver.Select(ctx, query)
		if err != nil {
			return nil, "", false, err
		}
		if len(rows) == 0 {
			if cursor.TaskID.IsNil() {
				return nil, "", true, nil
			}
			cursor.AfterTask = claimKey(req.PlanID, cursor.TaskID)
			cursor.TaskID = id.ID{}
			cursor.Revision = 0
		} else {
			row := rows[0]
			if operationErr := validateRecord(ctx, row); operationErr != nil {
				return nil, "", false, operationErr
			}
			claim, readErr := decodeClaim(row)
			if readErr != nil {
				return nil, "", false, readErr
			}
			if claim.PlanID != req.PlanID {
				return nil, "", false, planning.ErrConflict
			}
			cursor.TaskID = claim.TaskID
			cursor.Revision = claim.Revision
			if req.Source == "task_claim_history" {
				events = append(events, claimSourceEvent(row, claim, "", "task_claim_"+string(claim.State)))
			} else if claim.State == coordination.ClaimRecovering {
				cursor.Effects = true
			}
		}
	}
	next, err := encodeCoordinationCursor(cursor)
	return events, next, false, err
}
func (s *Repository) continuationSourceEvents(ctx context.Context, row Record, claim coordination.Claim) ([]coordination.Event, error) {
	if err := validateRecord(ctx, row); err != nil {
		return nil, err
	}
	var admission execution.EffectContinuation
	if row.Deleted || json.Unmarshal([]byte(row.Body), &admission) != nil || admission.Validate() != nil || row.Digest != admission.RequestDigest || admission.Request.Original.RunID != claim.RunID || admission.Request.Reservation.PlanID != claim.PlanID || admission.Request.Original.RunCall == nil || admission.Request.Original.RunCall.ClaimID != claim.ID {
		return nil, planning.ErrConflict
	}
	event := claimSourceEvent(row, claim, "", fmt.Sprintf("effect_%s_admitted", admission.Request.Action))
	event.ObservedAt = admission.At
	event.PredecessorDigest = admission.Request.PredecessorDigest
	result := []coordination.Event{event}
	completed, err := s.GetEffectContinuationResult(ctx, admission.Request)
	if err != nil {
		return nil, err
	}
	if completed != nil {
		event.SourceKind = kindEffectContinuationResult
		data, encodeErr := json.Marshal(completed)
		if encodeErr != nil {
			return nil, encodeErr
		}
		event.SourceDigest = evidence.Digest(data)
		event.EventID = evidence.Digest([]byte(event.Scope.Canonical() + "\x00" + event.PlanID.String() + "\x00" + event.SourceKind + "\x00" + row.ID))
		event.Type = fmt.Sprintf("effect_%s_resolved", admission.Request.Action)
		event.PredecessorDigest = admission.RequestDigest
		event.ObservedAt = completed.At
		artifact := completed.Evidence
		event.Artifact = &artifact
		result = append(result, event)
	}
	return result, nil
}
