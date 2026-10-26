package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/planning"
)

const (
	kindCoordinationEvent         = "coordination_event"
	kindCoordinationEventIdentity = "coordination_event_identity"
	kindCoordinationDelivery      = "coordination_delivery"
)

func coordinationEventKey(e coordination.Event) string {
	return e.PlanID.String() + ":" + evidence.Digest([]byte(e.SourceKind+"\x00"+e.SourceID))
}
func coordinationEventSequence(plan string, sequence int64) string {
	return fmt.Sprintf("%s:%020d", plan, sequence)
}
func decodeCoordinationEvent(ctx context.Context, row Record) (coordination.Event, error) {
	var e coordination.Event
	if err := validateRecord(ctx, row); err != nil {
		return e, err
	}
	if row.Deleted || len(row.Body) > 32<<10 || json.Unmarshal([]byte(row.Body), &e) != nil || e.Validate() != nil || row.Digest != e.Digest || row.ScopeCanon != e.Scope.Canonical() || row.RunID != e.PlanID.String() {
		return e, planning.ErrInvalid
	}
	return e, nil
}
func (s *Repository) appendCoordinationEvent(ctx context.Context, source coordination.Event) (bool, error) {
	frozen, err := coordination.SealEvent(source)
	if err != nil {
		return false, err
	}
	key := coordinationEventKey(frozen)
	cached, err := s.get(ctx, Query{Kind: kindCoordinationEventIdentity, ID: key})
	if err == nil {
		existing, readErr := decodeCoordinationEvent(ctx, cached)
		if readErr != nil {
			return false, readErr
		}
		frozen.Sequence = existing.Sequence
		frozen, readErr = coordination.SealEvent(frozen)
		if readErr != nil || frozen.Digest != existing.Digest {
			return false, planning.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(err, ErrAbsent) {
		return false, err
	}
	last, err := s.driver.Select(ctx, Query{Kind: kindCoordinationEvent, RunID: frozen.PlanID.String(), Reverse: true, Limit: 1})
	if err != nil {
		return false, err
	}
	frozen.Sequence = 1
	if len(last) > 0 {
		previous, readErr := decodeCoordinationEvent(ctx, last[0])
		if readErr != nil {
			return false, readErr
		}
		frozen.Sequence = previous.Sequence + 1
	}
	frozen, err = coordination.SealEvent(frozen)
	if err != nil {
		return false, err
	}
	row, err := record(kindCoordinationEvent, coordinationEventSequence(frozen.PlanID.String(), frozen.Sequence), frozen.Digest, frozen.PlanID.String(), frozen.Scope, frozen)
	if err != nil {
		return false, err
	}
	if operationErr := s.driver.Insert(ctx, row); operationErr != nil {
		return false, operationErr
	}
	identity, err := record(kindCoordinationEventIdentity, key, frozen.Digest, frozen.PlanID.String(), frozen.Scope, frozen)
	if err != nil {
		return false, err
	}
	if operationErr := s.driver.Insert(ctx, identity); operationErr != nil {
		return false, operationErr
	}
	return true, nil
}

// ListCoordinationEvents reads a finite private metadata page in index order.
func (s *Repository) ListCoordinationEvents(ctx context.Context, req coordination.EventPage) ([]coordination.Event, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if err := require(ctx, req.Scope); err != nil {
		return nil, err
	}
	after := ""
	if req.After > 0 {
		after = coordinationEventSequence(req.PlanID.String(), req.After)
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindCoordinationEvent, RunID: req.PlanID.String(), After: after, Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	result := make([]coordination.Event, 0, len(rows))
	for _, row := range rows {
		e, readErr := decodeCoordinationEvent(ctx, row)
		if readErr != nil {
			return nil, readErr
		}
		if e.PlanID != req.PlanID || e.Sequence != req.After+int64(len(result))+1 || row.ID != coordinationEventSequence(req.PlanID.String(), e.Sequence) {
			return nil, planning.ErrConflict
		}
		result = append(result, e)
	}
	return result, nil
}
func coordinationDeliveryKey(e coordination.Event, sink string) string {
	return coordinationEventKey(e) + ":" + evidence.Digest([]byte(sink))
}

// GetCoordinationDelivery returns the immutable receipt for an exact sink/event.
func (s *Repository) GetCoordinationDelivery(ctx context.Context, e coordination.Event, sink string) (*coordination.Delivery, error) {
	check := coordination.Delivery{Event: e, SinkID: sink, Reference: "read"}
	if err := check.Validate(); err != nil {
		return nil, err
	}
	if err := require(ctx, e.Scope); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, Query{Kind: kindCoordinationDelivery, ID: coordinationDeliveryKey(e, sink)})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var retained coordination.Delivery
	if row.Deleted || len(row.Body) > 64<<10 || json.Unmarshal([]byte(row.Body), &retained) != nil || retained.Validate() != nil || retained.Event.Digest != e.Digest || retained.SinkID != sink || row.Digest != evidence.Digest([]byte(row.Body)) || row.RunID != e.PlanID.String() {
		return nil, planning.ErrConflict
	}
	return &retained, nil
}

// AcknowledgeCoordinationEvent commits a trusted sink's exact authenticated
// receipt. Unknown delivery outcomes must never call this method.
func (s *Repository) AcknowledgeCoordinationEvent(ctx context.Context, receipt coordination.Delivery) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := require(ctx, receipt.Event.Scope); err != nil {
		return err
	}
	return s.planTransaction(ctx, receipt.Event.PlanID, func(tc context.Context, tx *Repository) error {
		row, err := tx.get(tc, Query{Kind: kindCoordinationEventIdentity, ID: coordinationEventKey(receipt.Event)})
		if err != nil {
			return err
		}
		event, err := decodeCoordinationEvent(tc, row)
		if err != nil {
			return err
		}
		if event.Digest != receipt.Event.Digest {
			return planning.ErrConflict
		}
		existing, err := tx.GetCoordinationDelivery(tc, event, receipt.SinkID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Reference != receipt.Reference {
				return planning.ErrConflict
			}
			return nil
		}
		data, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		row, err = record(kindCoordinationDelivery, coordinationDeliveryKey(event, receipt.SinkID), evidence.Digest(data), event.PlanID.String(), event.Scope, receipt)
		if err != nil {
			return err
		}
		return tx.driver.Insert(tc, row)
	})
}
