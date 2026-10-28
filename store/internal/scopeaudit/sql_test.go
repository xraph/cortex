package scopeaudit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/grove/driver"

	"github.com/xraph/cortex/store"
)

// fakeRows serves fixed rows of strings.
type fakeRows struct {
	rows [][]string
	at   int
}

func (r *fakeRows) Next() bool {
	r.at++
	return r.at <= len(r.rows)
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.at-1]
	if len(dest) != len(row) {
		return errors.New("column count mismatch")
	}
	for i, d := range dest {
		p, ok := d.(*string)
		if !ok {
			return errors.New("scan into a non-string")
		}
		*p = row[i]
	}
	return nil
}

func (*fakeRows) Columns() ([]string, error) { return nil, nil }
func (*fakeRows) Close() error               { return nil }
func (*fakeRows) Err() error                 { return nil }

// fakeCatalog answers the catalog query with tables and every table query
// with the rows planted for it, keeping the queries it was asked.
type fakeCatalog struct {
	columns map[string][]string
	rows    map[string][][]string
	queries []string
}

func (c *fakeCatalog) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	if query == "catalog" {
		var out [][]string
		for table, cols := range c.columns {
			for _, col := range cols {
				out = append(out, []string{table, col})
			}
		}
		return &fakeRows{rows: out}, nil
	}
	for table, rows := range c.rows {
		if strings.HasSuffix(query, quote(table)) {
			return &fakeRows{rows: rows}, nil
		}
	}
	return &fakeRows{}, nil
}

// A table keyed by run_id, cortex_run_progress, is inventoried by that
// key: a row planted under an unsafe scope is reported with its run id,
// and a safe one is not.
func TestSQLReportsUnsafeProgressRowByRunID(t *testing.T) {
	db := &fakeCatalog{
		columns: map[string][]string{
			"cortex_run_progress": {"run_id", "owner", "progress", "scope_l0", "scope_l1", "scope_l2", "scope_extra", "scope_canon"},
		},
		rows: map[string][][]string{
			"cortex_run_progress": {
				{"arun_safe", "workspace=w", "workspace=w", "", "", "{}"},
				{"arun_unsafe", "workspace=w", "workspace=w", "", "", `{"region":"hidden"}`},
			},
		},
	}
	var issues []store.ScopeIssue
	if err := SQL(context.Background(), db, "catalog", func(issue store.ScopeIssue) error {
		issues = append(issues, issue)
		return nil
	}); err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(issues) != 1 || issues[0].Resource != "cortex_run_progress" || issues[0].ID != "arun_unsafe" {
		t.Fatalf("issues = %+v, want one for arun_unsafe", issues)
	}
	keyed := false
	for _, q := range db.queries {
		if strings.Contains(q, `CAST("run_id" AS TEXT)`) {
			keyed = true
		}
	}
	if !keyed {
		t.Fatalf("the progress table was not read by run_id: %q", db.queries)
	}
}
