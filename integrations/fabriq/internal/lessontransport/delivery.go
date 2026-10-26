package lessontransport

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/fabriqerr"
	"github.com/xraph/fabriq/core/query"

	"github.com/xraph/cortex/learning"
)

// Writer is the installed Fabriq toolkit command boundary.
type Writer interface {
	Remember(context.Context, agent.RememberRequest) (command.Result, error)
}

// Read resolves one aggregate through authoritative tenant-scoped storage.
func Read(ctx context.Context, fabric query.Fabric, entity, aggregate string) (Row, error) {
	var rows []map[string]any
	if err := fabric.Relational().GetMany(ctx, entity, []string{aggregate}, &rows); err != nil {
		return Row{}, err
	}
	if len(rows) == 0 {
		return Row{}, fabriqerr.ErrNotFound
	}
	if len(rows) != 1 {
		return Row{}, learning.ErrIneligible
	}
	return Decode(rows[0])
}

// Reconcile uses the stable aggregate and expected version, then reads the
// authoritative row after either a success or an ambiguous write response.
func Reconcile(ctx context.Context, writer Writer, fabric query.Fabric, entity, tenant string, publication learning.Publication, previous func(context.Context) (learning.Publication, error)) (Row, error) {
	row, readErr := Read(ctx, fabric, entity, publication.AggregateID)
	if readErr == nil && row.Matches(publication, tenant) {
		return row, nil
	}
	if readErr != nil && !errors.Is(readErr, fabriqerr.ErrNotFound) {
		return Row{}, learning.ErrUnavailable
	}
	operation, version := "create", int64(0)
	var prior *learning.Publication
	if readErr == nil {
		if publication.Operation != "revoke" || previous == nil {
			return Row{}, learning.ErrConflict
		}
		value, err := previous(ctx)
		if err != nil {
			return Row{}, err
		}
		if value.Operation != "publish" || !row.Matches(value, tenant) {
			return Row{}, learning.ErrConflict
		}
		prior, operation, version = &value, "update", row.Version
	}
	payload, err := json.Marshal(For(publication))
	if err != nil {
		return Row{}, learning.ErrInvalid
	}
	_, writeErr := writer.Remember(ctx, agent.RememberRequest{Entity: entity, Op: operation, AggID: publication.AggregateID, ExpectedVersion: &version, Payload: payload})
	row, readErr = Read(ctx, fabric, entity, publication.AggregateID)
	if readErr != nil {
		var denied *agent.WriteError
		if errors.As(writeErr, &denied) && denied.Code == "not_allowed" {
			return Row{}, learning.ErrDenied
		}
		return Row{}, learning.ErrUnavailable
	}
	if !row.Matches(publication, tenant) {
		if prior != nil && row.Version == version && row.Matches(*prior, tenant) && writeErr != nil {
			var denied *agent.WriteError
			if errors.As(writeErr, &denied) && denied.Code == "not_allowed" {
				return Row{}, learning.ErrDenied
			}
			return Row{}, learning.ErrUnavailable
		}
		return Row{}, learning.ErrConflict
	}
	return row, nil
}
