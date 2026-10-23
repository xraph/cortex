package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/xraph/grove/drivers/pgdriver"
	"github.com/xraph/grove/migrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store/internal/assurance"
)

type assuranceQueries interface {
	NewSelect(...any) *pgdriver.SelectQuery
	NewInsert(any) *pgdriver.InsertQuery
	NewUpdate(any) *pgdriver.UpdateQuery
}

type assuranceDriver struct {
	store       *Store
	transaction *pgdriver.PgTx
}

func (d assuranceDriver) queries() assuranceQueries {
	if d.transaction != nil {
		return d.transaction
	}
	return d.store.pgdb
}

func (d assuranceDriver) Insert(ctx context.Context, row assurance.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := d.queries().NewInsert(&row).Exec(ctx)
	if isUniqueViolation(err) {
		return assurance.ErrExists
	}
	return err
}

func (d assuranceDriver) Select(ctx context.Context, query assurance.Query) ([]assurance.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	var rows []assurance.Record
	order, after := "id ASC", "id > ?"
	if query.Reverse {
		order, after = "id DESC", "id < ?"
	}
	q := d.queries().NewSelect(&rows).Where("kind = ?", query.Kind).Where("scope_canon = ?", scope.Canonical()).Limit(query.Limit).OrderExpr(order)
	for _, predicate := range scopePredicates(scope, true) {
		q = q.Where(predicate.Column+" = ?", predicate.Value)
	}
	if query.ID != "" {
		q = q.Where("id = ?", query.ID)
	}
	if query.Digest != "" {
		q = q.Where("digest = ?", query.Digest)
	}
	if query.RunID != "" {
		q = q.Where("run_id = ?", query.RunID)
	}
	if query.After != "" {
		q = q.Where(after, query.After)
	}
	if d.transaction != nil {
		q = q.ForShare()
	}
	if err = q.Scan(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return rows, nil
}

func (d assuranceDriver) ReplaceEvidence(ctx context.Context, row assurance.Record, expectedBody string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	q := d.queries().NewUpdate((*assurance.Record)(nil)).Set("body = ?", row.Body).Set("deleted = ?", true).
		Where("kind = ?", assurance.KindEvidence).Where("id = ?", row.ID).Where("scope_canon = ?", scope.Canonical()).Where("body = ?", expectedBody).Where("deleted = ?", false)
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

func init() {
	Migrations.MustRegister(&migrate.Migration{Name: "create_assurance_records", Version: "20261018000003", Comment: "Immutable evidence and verification records",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, assurance.SQLMigration)
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "DROP TABLE IF EXISTS cortex_assurance_records")
			return err
		},
	})
}
