package postgres

import (
	"github.com/xraph/cortex"
)

// indexedLevels is how many scope levels get their own indexed column.
// Levels past this land in the scope_extra JSONB, which is queryable but
// not indexed. Three covers every host we know about; raising it later
// means a migration, so it is deliberately generous.
const indexedLevels = 3

// scopePredicate is one equality clause against a scope column.
type scopePredicate struct {
	Column string
	Value  string
}

// scopeColumns flattens a scope into the three indexed column values plus
// an overflow map. Absent levels are the empty string, never NULL, so that
// they stay comparable and indexable.
func scopeColumns(s cortex.Scope) (l0, l1, l2 string, extra map[string]string) {
	cols := make([]string, indexedLevels)
	extra = make(map[string]string)

	for i, lvl := range s.Levels {
		encoded := lvl.Key + "=" + lvl.Value
		if i < indexedLevels {
			//nolint:gosec // slice bounds check via i < indexedLevels
			cols[i] = encoded
			continue
		}
		extra[lvl.Key] = lvl.Value
	}
	return cols[0], cols[1], cols[2], extra
}

// scopePredicates builds the WHERE clauses for a scope filter.
//
// Prefix matching (exact = false) is the default: one equality per level
// the caller actually supplied, and nothing for the levels they left off,
// so a workspace-only filter matches every project inside it. This follows
// the convention that an omitted project returns results across the whole
// workspace.
//
// Exact matching additionally pins every unsupplied indexed column to the
// empty string, which narrows the read to rows stored at precisely this
// depth.
func scopePredicates(s cortex.Scope, exact bool) []scopePredicate {
	cols := []string{"scope_l0", "scope_l1", "scope_l2"}
	l0, l1, l2, _ := scopeColumns(s)
	vals := []string{l0, l1, l2}

	preds := make([]scopePredicate, 0, indexedLevels)
	for i := range cols {
		if vals[i] == "" && !exact {
			continue
		}
		preds = append(preds, scopePredicate{Column: cols[i], Value: vals[i]})
	}
	return preds
}
