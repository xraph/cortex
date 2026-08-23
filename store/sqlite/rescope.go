package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/xraph/cortex"
)

// tableShape records which of the legacy identifier columns (name,
// app_id, tenant_id, run_id) a table actually has. Not every table has
// all of them: cortex_agents carries name+app_id,
// cortex_runs/cortex_memories/cortex_checkpoints carry tenant_id, and
// cortex_steps/cortex_tool_calls carry neither -- they inherit their
// scope from the run they belong to instead. A missing column is
// substituted with an empty string in the SELECT, so one query shape covers
// every table without assuming a uniform schema.
type tableShape struct {
	hasName     bool
	hasAppID    bool
	hasTenantID bool
	hasRunID    bool
}

// legacyRow is one row awaiting a scope, with whichever legacy identifier
// columns its table happened to carry. Fields for columns the table
// doesn't have are left as the empty string.
type legacyRow struct {
	Table    string
	ID       string
	Name     string
	AppID    string
	TenantID string
	RunID    string
}

func (r legacyRow) key() string { return r.Table + "|" + r.ID }

// rawScope is a resolved scope in the same shape the scope columns are
// stored in, so a row that inherits its scope from a parent can be
// written without round-tripping through a cortex.Scope value.
type rawScope struct {
	L0, L1, L2 string
	Extra      string // JSON-encoded, matching scopeColumns' sqlite shape
	Canon      string
}

func toRawScope(sc cortex.Scope) rawScope {
	l0, l1, l2, extra := scopeColumns(sc)
	return rawScope{L0: l0, L1: l1, L2: l2, Extra: extra, Canon: sc.Canonical()}
}

// rescopeLegacyRows fills in scope columns for rows created before
// v1.8.0. The ordering is the design: scan everything, decide everything,
// check for collisions, and only then write. A half-rescoped database has
// some rows reachable and some orphaned with nothing to tell them apart,
// which is strictly worse than a migration that refused to start.
func (s *Store) rescopeLegacyRows(ctx context.Context, o cortex.MigrateOptions) error {
	shapes, err := s.discoverScopedTables(ctx)
	if err != nil {
		return fmt.Errorf("discover scoped tables: %w", err)
	}

	rows, err := s.scanUnscoped(ctx, shapes)
	if err != nil {
		return fmt.Errorf("scan unscoped rows: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	// A row that only carries a run_id (a step or tool call) never calls
	// the Rescoper itself -- it inherits whatever its parent run resolves
	// to. So a rescoper is only required when at least one row needs to
	// be resolved directly: an unscoped run is itself a direct row (it
	// has no run_id of its own), so this still catches the case where a
	// dependent's parent is unscoped too.
	if hasDirectRows(rows) && o.Rescoper == nil {
		return fmt.Errorf("%w: %d rows across %d tables need one",
			cortex.ErrNoRescoper, len(rows), countTables(rows))
	}

	resolved, err := s.resolveScopes(ctx, rows, o.Rescoper)
	if err != nil {
		return err
	}

	if err := detectCollisions(rows, resolved); err != nil {
		return err
	}

	return s.applyScopes(ctx, rows, resolved)
}

// discoverScopedTables finds every cortex_* table that actually has a
// scope_canon column, and which legacy identifier columns each one
// carries. Deriving this at runtime -- instead of a hardcoded table list
// -- keeps the pass correct against a partially migrated database, which
// is exactly the state it's most likely to meet: later phases add
// scope_canon to more tables over time, and a hardcoded list would fail
// on the first one that isn't there yet.
func (s *Store) discoverScopedTables(ctx context.Context) (map[string]tableShape, error) {
	names, err := s.cortexTableNames(ctx)
	if err != nil {
		return nil, err
	}

	shapes := make(map[string]tableShape)
	for _, table := range names {
		cols, colsErr := s.tableColumns(ctx, table)
		if colsErr != nil {
			return nil, colsErr
		}
		if !cols["scope_canon"] {
			continue
		}
		shapes[table] = tableShape{
			hasName:     cols["name"],
			hasAppID:    cols["app_id"],
			hasTenantID: cols["tenant_id"],
			hasRunID:    cols["run_id"],
		}
	}
	return shapes, nil
}

func (s *Store) cortexTableNames(ctx context.Context) ([]string, error) {
	rows, err := s.sdb.Query(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'cortex\_%' ESCAPE '\'`)
	if err != nil {
		return nil, fmt.Errorf("list cortex tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("scan table name: %w", scanErr)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// tableColumns reports the columns table has, via PRAGMA table_info.
// table is always sourced from sqlite_master in the same call, never
// caller input, so building the PRAGMA statement by concatenation is
// safe -- PRAGMA doesn't accept bound parameters for table names anyway.
func (s *Store) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.sdb.Query(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols := make(map[string]bool)
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue any
			pk        int
		)
		if scanErr := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); scanErr != nil {
			return nil, fmt.Errorf("scan columns of %s: %w", table, scanErr)
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// scanUnscoped selects every row with an empty scope_canon from each
// table in shapes, substituting an empty string for whichever legacy identifier
// columns that particular table doesn't have.
func (s *Store) scanUnscoped(ctx context.Context, shapes map[string]tableShape) ([]legacyRow, error) {
	var rows []legacyRow
	for table, shape := range shapes {
		cols := []string{
			"id",
			selectOrEmpty("name", shape.hasName),
			selectOrEmpty("app_id", shape.hasAppID),
			selectOrEmpty("tenant_id", shape.hasTenantID),
			selectOrEmpty("run_id", shape.hasRunID),
		}
		query := `SELECT ` + strings.Join(cols, ", ") + ` FROM ` + table + ` WHERE scope_canon = ''`

		tableRows, err := s.scanTable(ctx, table, query)
		if err != nil {
			return nil, err
		}
		rows = append(rows, tableRows...)
	}
	return rows, nil
}

func (s *Store) scanTable(ctx context.Context, table, query string) ([]legacyRow, error) {
	rs, err := s.sdb.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", table, err)
	}
	defer func() { _ = rs.Close() }()

	var rows []legacyRow
	for rs.Next() {
		r := legacyRow{Table: table}
		if scanErr := rs.Scan(&r.ID, &r.Name, &r.AppID, &r.TenantID, &r.RunID); scanErr != nil {
			return nil, fmt.Errorf("scan row in %s: %w", table, scanErr)
		}
		rows = append(rows, r)
	}
	return rows, rs.Err()
}

func selectOrEmpty(col string, present bool) string {
	if present {
		return col
	}
	return `'' AS ` + col
}

// resolveScopes decides a target scope for every unscoped row. Rows that
// carry their own legacy identifier (app_id and/or tenant_id, or neither
// column exists at all) go straight through the Rescoper, once per
// distinct (appID, tenantID) pair. Rows that carry only a run_id --
// cortex_steps and cortex_tool_calls -- have no legacy identifier of
// their own; they inherit whatever scope their parent run resolves to,
// so a scoped ListSteps/ListToolCalls under the run's scope still finds
// them afterward.
func (s *Store) resolveScopes(ctx context.Context, rows []legacyRow, r cortex.Rescoper) (map[string]rawScope, error) {
	resolved := make(map[string]rawScope, len(rows))
	byAppTenant := make(map[[2]string]rawScope)
	runScope := make(map[string]rawScope)
	var dependents []legacyRow

	for _, row := range rows {
		if isDependentRow(row) {
			dependents = append(dependents, row)
			continue
		}

		key := [2]string{row.AppID, row.TenantID}
		rs, ok := byAppTenant[key]
		if !ok {
			if r == nil {
				// hasDirectRows in the caller should have already turned
				// this into ErrNoRescoper; guarded here too so a future
				// change to that check fails loud instead of panicking.
				return nil, fmt.Errorf("%w: row %s in %s needs one", cortex.ErrNoRescoper, row.ID, row.Table)
			}
			sc, rErr := r.Rescope(ctx, row.AppID, row.TenantID)
			if rErr != nil {
				return nil, fmt.Errorf("rescope (app=%q tenant=%q): %w", row.AppID, row.TenantID, rErr)
			}
			if vErr := cortex.ValidateRescopedScope(sc); vErr != nil {
				return nil, fmt.Errorf("rescope (app=%q tenant=%q): %w", row.AppID, row.TenantID, vErr)
			}
			rs = toRawScope(sc)
			byAppTenant[key] = rs
		}
		resolved[row.key()] = rs
		if row.Table == "cortex_runs" {
			runScope[row.ID] = rs
		}
	}

	if len(dependents) == 0 {
		return resolved, nil
	}
	if err := s.resolveDependents(ctx, dependents, runScope, resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// resolveDependents fills in resolved for rows that inherit their scope
// from a parent run. runScope already holds every run resolved in this
// same pass; any run_id not in there belongs to a run that was already
// scoped before this pass ran, so its current scope columns are read
// straight from the database instead of asked of the Rescoper again.
func (s *Store) resolveDependents(ctx context.Context, dependents []legacyRow, runScope, resolved map[string]rawScope) error {
	missing := make(map[string]struct{})
	for _, row := range dependents {
		if _, ok := runScope[row.RunID]; !ok {
			missing[row.RunID] = struct{}{}
		}
	}

	if len(missing) > 0 {
		ids := make([]string, 0, len(missing))
		for id := range missing {
			ids = append(ids, id)
		}
		fetched, err := s.fetchRunScopes(ctx, ids)
		if err != nil {
			return err
		}
		for id, rs := range fetched {
			runScope[id] = rs
		}
	}

	for _, row := range dependents {
		rs, ok := runScope[row.RunID]
		if !ok {
			return fmt.Errorf("rescope: %s row %s references run %s, which has no resolvable scope",
				row.Table, row.ID, row.RunID)
		}
		resolved[row.key()] = rs
	}
	return nil
}

// fetchRunScopes reads the current scope columns for a set of runs
// directly, for runs that are already scoped and so weren't part of the
// pass's own scan.
func (s *Store) fetchRunScopes(ctx context.Context, ids []string) (map[string]rawScope, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT id, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon FROM cortex_runs WHERE id IN (` +
		strings.Join(placeholders, ",") + `)`

	rows, err := s.sdb.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch parent run scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]rawScope, len(ids))
	for rows.Next() {
		var id string
		var rs rawScope
		if scanErr := rows.Scan(&id, &rs.L0, &rs.L1, &rs.L2, &rs.Extra, &rs.Canon); scanErr != nil {
			return nil, fmt.Errorf("scan run scope: %w", scanErr)
		}
		// A run this pass didn't resolve itself must already carry a
		// real scope -- the run_id foreign key means a step or tool
		// call can never point at a run that doesn't exist at all, so
		// an empty canon here would mean the scan missed an unscoped
		// run, which should be impossible.
		if rs.Canon == "" {
			return nil, fmt.Errorf("rescope: run %s has no resolvable scope", id)
		}
		out[id] = rs
	}
	return out, rows.Err()
}

// detectCollisions finds rows that would violate a (scope_canon, name)
// uniqueness expectation once written, and reports them before anything
// is written. Letting a later unique index reject them mid-write would be
// a data-loss surprise; this turns it into a fix-your-rescoper message.
// Rows without a name column (cortex_runs, cortex_steps, and friends)
// never participate -- there is nothing to collide on.
func detectCollisions(rows []legacyRow, resolved map[string]rawScope) error {
	seen := make(map[[3]string]string) // table+canon+name -> first row id
	for _, r := range rows {
		if r.Name == "" {
			continue
		}
		canon := resolved[r.key()].Canon
		k := [3]string{r.Table, canon, r.Name}
		if first, ok := seen[k]; ok {
			return fmt.Errorf(
				"rescope collision: %s rows %s and %s both map to scope %q with name %q; "+
					"the rescoper must keep them in distinct scopes",
				r.Table, first, r.ID, canon, r.Name)
		}
		seen[k] = r.ID
	}
	return nil
}

// applyScopes writes every resolved scope inside one transaction, so a
// mid-write failure rolls back whole rather than leaving some rows
// rescoped and others not.
func (s *Store) applyScopes(ctx context.Context, rows []legacyRow, resolved map[string]rawScope) error {
	tx, err := s.sdb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rescope transaction: %w", err)
	}
	// After a successful Commit, Rollback is a documented no-op; before
	// one, its error can't be acted on any further than the failure that
	// triggered this defer already is.
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit, unactionable otherwise

	for _, r := range rows {
		rs := resolved[r.key()]
		query := `UPDATE ` + r.Table + ` SET scope_l0 = ?, scope_l1 = ?, scope_l2 = ?, scope_extra = ?, scope_canon = ? WHERE id = ?`
		if _, execErr := tx.Exec(ctx, query, rs.L0, rs.L1, rs.L2, rs.Extra, rs.Canon, r.ID); execErr != nil {
			return fmt.Errorf("rescope %s row %s: %w", r.Table, r.ID, execErr)
		}
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("commit rescope transaction: %w", commitErr)
	}
	return nil
}

func countTables(rows []legacyRow) int {
	t := make(map[string]struct{})
	for _, r := range rows {
		t[r.Table] = struct{}{}
	}
	return len(t)
}

// isDependentRow reports whether row must inherit its scope from a parent
// run rather than being resolved directly: it carries a run_id and
// neither of the identifier columns a direct row would use.
func isDependentRow(row legacyRow) bool {
	return row.RunID != "" && row.AppID == "" && row.TenantID == ""
}

// hasDirectRows reports whether any row in rows needs to be resolved
// directly through the Rescoper, as opposed to inheriting a parent's
// scope. An unscoped run is a direct row itself, so this still requires
// a rescoper whenever a dependent's own parent is unscoped.
func hasDirectRows(rows []legacyRow) bool {
	for _, r := range rows {
		if !isDependentRow(r) {
			return true
		}
	}
	return false
}
