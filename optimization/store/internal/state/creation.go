package state

import (
	"context"
	"reflect"

	"github.com/xraph/cortex/optimization"
)

// GetCreationReceipt retrieves one exact-scope receipt without loading a catalog.
func (s *Store) GetCreationReceipt(ctx context.Context, commandKey string) (optimization.CreationReceipt, error) {
	var empty optimization.CreationReceipt
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, err := s.loadResource(ctx, key, RecordKey{Kind: "creation_receipts", Key: commandKey})
	if err != nil {
		return empty, err
	}
	if data != nil {
		if receipt, exists := data.CreationReceipts[commandKey]; exists {
			return clone(receipt)
		}
	}
	return empty, optimization.ErrNotFound
}

func (tx *transaction) CreateExperimentCommand(command optimization.CreationCommand) (optimization.CreationReceipt, error) {
	var empty optimization.CreationReceipt
	if !tx.active {
		return empty, optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return empty, err
	}
	scope := command.Bundle.Experiment.Spec.Scope
	if scope.Canonical() != tx.scope || command.Bundle.Experiment.Spec.Requester != command.Actor || command.Bundle.ParentLease != nil || command.Bundle.Experiment.Spec.Parent != nil {
		return empty, optimization.ErrPolicyDenied
	}
	receipt, err := optimization.SealCreationReceipt(optimization.CreationReceipt{Key: command.Key, Scope: scope, Actor: command.Actor, Selection: command.Selection, ExperimentID: command.Bundle.Experiment.ID, CreatedAt: tx.now})
	if err != nil {
		return empty, err
	}
	if prior, exists := tx.data.CreationReceipts[command.Key]; exists {
		sealed, sealErr := optimization.SealCreationReceipt(prior)
		if sealErr != nil || !reflect.DeepEqual(sealed, prior) || prior.SpecDigest == "" {
			return empty, optimization.ErrIncompleteEvidence
		}
		if prior.InputDigest != receipt.InputDigest {
			return empty, optimization.ErrConflict
		}
		return clone(prior)
	}
	if validationErr := optimization.ValidateCreationBundle(command.Selection, command.Bundle); validationErr != nil {
		return empty, validationErr
	}
	if creationErr := tx.CreateExperiment(command.Bundle); creationErr != nil {
		return empty, creationErr
	}
	receipt.SpecDigest = tx.data.Experiments[receipt.ExperimentID.String()].SpecDigest
	if tx.data.CreationReceipts == nil {
		tx.data.CreationReceipts = map[string]optimization.CreationReceipt{}
	}
	tx.data.CreationReceipts[command.Key] = receipt
	return clone(receipt)
}
