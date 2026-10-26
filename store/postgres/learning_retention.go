package postgres

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/assurance"
)

func (d assuranceDriver) EraseLessonPayloads(ctx context.Context, identifier id.ID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if d.transaction == nil || identifier.IsNil() || identifier.Prefix() != "lesson" {
		return assurance.ErrExists
	}
	for _, kind := range assurance.LessonPayloadKinds() {
		q := d.queries().NewUpdate((*assurance.Record)(nil)).Set("body = ?", "{}").Set("deleted = ?", true).
			Where("kind = ?", kind).Where("scope_canon = ?", scope.Canonical()).Where("id LIKE ?", identifier.String()+"/%")
		for _, predicate := range scopePredicates(scope, true) {
			q = q.Where(predicate.Column+" = ?", predicate.Value)
		}
		if _, err = q.Exec(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (d assuranceDriver) FenceLessonRevision(ctx context.Context, key, digest string) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if d.transaction == nil {
		return assurance.ErrExists
	}
	q := d.queries().NewUpdate((*assurance.Record)(nil)).Set("body = body").Where("kind = ?", "lesson").Where("id = ?", key).Where("digest = ?", digest).Where("deleted = ?", false).Where("scope_canon = ?", scope.Canonical())
	for _, predicate := range scopePredicates(scope, true) {
		q = q.Where(predicate.Column+" = ?", predicate.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return assurance.ErrExists
	}
	return nil
}
