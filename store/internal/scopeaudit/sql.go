// Package scopeaudit implements read-only stored-scope inventory shared by adapters.
package scopeaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/xraph/grove/driver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store"
)

// Queryer is the SQL read surface required by inventory.
type Queryer interface {
	Query(context.Context, string, ...any) (driver.Rows, error)
}

// Issue returns a finding only when a stored identity is unsafe.
func Issue(resource, id, canonical string, indexed [3]string, extra string) *store.ScopeIssue {
	var overflow map[string]string
	err := json.Unmarshal([]byte(extra), &overflow)
	if err != nil {
		err = cortex.ErrInvalidScope
	} else {
		_, err = cortex.DecodeStoredScope(canonical, indexed, len(overflow) > 0)
	}
	if err == nil {
		return nil
	}
	return &store.ScopeIssue{Resource: resource, ID: id, Canonical: canonical, Indexed: indexed, Extra: extra, Reason: err.Error()}
}

// SQL discovers scoped tables from a driver-specific catalog query, then streams
// identity columns only. Catalog identifiers are quoted before use in SQL.
func SQL(ctx context.Context, db Queryer, columnsQuery string, visit func(store.ScopeIssue) error) error {
	if visit == nil {
		return fmt.Errorf("cortex: scope inventory callback is required")
	}
	rows, err := db.Query(ctx, columnsQuery)
	if err != nil {
		return err
	}
	tables := make(map[string]map[string]bool)
	for rows.Next() {
		var table, column string
		if scanErr := rows.Scan(&table, &column); scanErr != nil {
			_ = rows.Close()
			return scanErr
		}
		if tables[table] == nil {
			tables[table] = make(map[string]bool)
		}
		tables[table][column] = true
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, table := range names {
		columns := tables[table]
		if !columns["scope_canon"] {
			continue
		}
		// Most tables are keyed by id. cortex_a2a_pending_asks is keyed by
		// its reply_with token and cortex_run_progress by its run_id.
		key := "id"
		for _, candidate := range []string{"id", "reply_with", "run_id"} {
			if columns[candidate] {
				key = candidate
				break
			}
		}
		for _, required := range []string{key, "scope_l0", "scope_l1", "scope_l2", "scope_extra"} {
			if !columns[required] {
				return fmt.Errorf("cortex: scope inventory needs completed schema for %s", table)
			}
		}
		if err := inspectTable(ctx, db, table, key, visit); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func quote(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func inspectTable(ctx context.Context, db Queryer, table, key string, visit func(store.ScopeIssue) error) error {
	// Identifiers come from the database catalog and are quoted, never SQL input.
	query := `SELECT CAST(` + quote(key) + ` AS TEXT), scope_canon, scope_l0, scope_l1, scope_l2, CAST(scope_extra AS TEXT) FROM ` + quote(table)
	rows, err := db.Query(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var id, canon, extra string
		var indexed [3]string
		if err := rows.Scan(&id, &canon, &indexed[0], &indexed[1], &indexed[2], &extra); err != nil {
			return err
		}
		if finding := Issue(table, id, canon, indexed, extra); finding != nil {
			if err := visit(*finding); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return ctx.Err()
}
