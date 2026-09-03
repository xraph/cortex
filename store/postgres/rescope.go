package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
)

// tableShape records which of the legacy identifier columns (name,
// app_id, tenant_id, run_id, agent_id, kind, key) a table actually has.
// Not every table has all of them: cortex_agents carries name+app_id,
// cortex_runs/cortex_memories/cortex_checkpoints carry tenant_id, and
// cortex_steps/cortex_tool_calls carry neither -- they inherit their
// scope from the run they belong to instead. agent_id/kind/key exist
// only to support cortex_memories' working-memory collision check.
// A missing column is substituted with an empty string in the SELECT,
// so one query shape covers every table without assuming a uniform
// schema.
type tableShape struct {
	hasName     bool
	hasAppID    bool
	hasTenantID bool
	hasRunID    bool
	hasAgentID  bool
	hasKind     bool
	hasKey      bool
}

// isDependent reports whether a row of this shape must inherit its scope
// from a parent run rather than being resolved directly through the
// Rescoper: it has a run_id and neither of the identifier columns a
// direct row would use. This is a property of the TABLE, not of any one
// row's values -- cortex_checkpoints has both run_id and tenant_id, so a
// checkpoint whose tenant_id happens to be blank must still be resolved
// directly, the same as every other checkpoint of the same run. Deciding
// this from a row's values instead of its table's shape would let two
// checkpoints of one run land in different scopes.
func (t tableShape) isDependent() bool {
	return t.hasRunID && !t.hasAppID && !t.hasTenantID
}

// legacyRow is one row awaiting a scope, with whichever legacy identifier
// columns its table happened to carry. Fields for columns the table
// doesn't have are left as the empty string.
//
// ID is `any` rather than `string` because these tables do not agree on a
// key type: cortex_memories.id is BIGSERIAL, while every other scoped
// table uses a TEXT TypeID. Keeping the scanned value in its real type
// means the driver binds it back unchanged.
//
// A string would in fact work today: pgx wraps *string via
// TryWrapBuiltinTypeScanPlan so Int8Codec accepts it, and text format is
// chosen for the write-back. That is an implementation detail of the
// driver's scan-plan resolution, not a documented contract, and relying
// on it buys nothing here.
type legacyRow struct {
	Table     string
	ID        any
	Name      string
	AppID     string
	TenantID  string
	RunID     string
	AgentID   string
	Kind      string
	Key       string
	Dependent bool
}

func (r legacyRow) key() string { return r.Table + "|" + fmt.Sprint(r.ID) }

// rawScope is a resolved scope in the same shape the scope columns are
// stored in, so a row that inherits its scope from a parent can be
// written without round-tripping through a cortex.Scope value.
type rawScope struct {
	L0, L1, L2 string
	Extra      map[string]string
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

	if err := s.detectCollisions(ctx, rows, resolved); err != nil {
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
	rows, err := s.pgdb.Query(ctx, `
SELECT table_name, column_name
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name LIKE 'cortex\_%' ESCAPE '\'
  AND column_name = ANY($1)`,
		[]string{"scope_canon", "id", "name", "app_id", "tenant_id", "run_id", "agent_id", "kind", "key"})
	if err != nil {
		return nil, fmt.Errorf("list cortex table columns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols := make(map[string]map[string]bool)
	for rows.Next() {
		var table, column string
		if scanErr := rows.Scan(&table, &column); scanErr != nil {
			return nil, fmt.Errorf("scan column info: %w", scanErr)
		}
		if cols[table] == nil {
			cols[table] = make(map[string]bool)
		}
		cols[table][column] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	shapes := make(map[string]tableShape)
	for table, c := range cols {
		if !c["scope_canon"] {
			continue
		}
		// A scoped table with no id column is one this pass cannot key a
		// row by, and also one that never needs to: the tables predating
		// scope all carry a TypeID id, while a table keyed by something
		// else (cortex_a2a_pending_asks, keyed by its reply-with token)
		// was born with its scope columns and has no legacy rows to
		// backfill. Skipping it is therefore not a gap.
		if !c["id"] {
			continue
		}
		shapes[table] = tableShape{
			hasName:     c["name"],
			hasAppID:    c["app_id"],
			hasTenantID: c["tenant_id"],
			hasRunID:    c["run_id"],
			hasAgentID:  c["agent_id"],
			hasKind:     c["kind"],
			hasKey:      c["key"],
		}
	}
	return shapes, nil
}

// scanUnscoped selects every row with an empty scope_canon from each
// table in shapes, substituting an empty string for whichever legacy
// identifier columns that particular table doesn't have.
func (s *Store) scanUnscoped(ctx context.Context, shapes map[string]tableShape) ([]legacyRow, error) {
	var rows []legacyRow
	for table, shape := range shapes {
		query := `SELECT id, ` +
			selectOrEmpty("name", shape.hasName) + `, ` +
			selectOrEmpty("app_id", shape.hasAppID) + `, ` +
			selectOrEmpty("tenant_id", shape.hasTenantID) + `, ` +
			selectOrEmpty("run_id", shape.hasRunID) + `, ` +
			selectOrEmpty("agent_id", shape.hasAgentID) + `, ` +
			selectOrEmpty("kind", shape.hasKind) + `, ` +
			selectOrEmpty(`"key"`, shape.hasKey) +
			` FROM ` + table + ` WHERE scope_canon = ''`

		tableRows, err := s.scanTable(ctx, table, query, shape.isDependent())
		if err != nil {
			return nil, err
		}
		rows = append(rows, tableRows...)
	}
	return rows, nil
}

func (s *Store) scanTable(ctx context.Context, table, query string, dependent bool) ([]legacyRow, error) {
	rs, err := s.pgdb.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", table, err)
	}
	defer func() { _ = rs.Close() }()

	var rows []legacyRow
	for rs.Next() {
		r := legacyRow{Table: table, Dependent: dependent}
		if scanErr := rs.Scan(&r.ID, &r.Name, &r.AppID, &r.TenantID, &r.RunID, &r.AgentID, &r.Kind, &r.Key); scanErr != nil {
			return nil, fmt.Errorf("scan row in %s: %w", table, scanErr)
		}
		rows = append(rows, r)
	}
	return rows, rs.Err()
}

// selectOrEmpty selects col if the table has it, or a literal empty
// string aliased to the same name otherwise. expr is either a bare
// column name or (for "key", which the rest of this package always
// quotes -- see memory.go's LoadWorking) a pre-quoted identifier; the
// alias always uses the unquoted form so Scan's column order stays
// predictable either way.
func selectOrEmpty(expr string, present bool) string {
	if !present {
		return `'' AS ` + trimQuotes(expr)
	}
	return expr
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// resolveScopes decides a target scope for every unscoped row. Rows that
// carry their own legacy identifier (app_id and/or tenant_id, or neither
// column exists at all) go straight through the Rescoper, once per
// distinct (appID, tenantID) pair. Rows whose table shape marks them
// dependent -- cortex_steps and cortex_tool_calls -- have no legacy
// identifier of their own; they inherit whatever scope their parent run
// resolves to, so a scoped ListSteps/ListToolCalls under the run's scope
// still finds them afterward.
func (s *Store) resolveScopes(ctx context.Context, rows []legacyRow, r cortex.Rescoper) (map[string]rawScope, error) {
	resolved := make(map[string]rawScope, len(rows))
	byAppTenant := make(map[[2]string]rawScope)
	runScope := make(map[string]rawScope)
	var dependents []legacyRow

	for _, row := range rows {
		if row.Dependent {
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
				return nil, fmt.Errorf("%w: row %v in %s needs one", cortex.ErrNoRescoper, row.ID, row.Table)
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
			runScope[fmt.Sprint(row.ID)] = rs
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
			return fmt.Errorf("rescope: %s row %v references run %s, which has no resolvable scope",
				row.Table, row.ID, row.RunID)
		}
		resolved[row.key()] = rs
	}
	return nil
}

// fetchRunScopes reads the current scope columns for a set of runs
// directly, for runs that are already scoped and so weren't part of the
// pass's own scan. cortex_runs.id is always TEXT, unlike
// cortex_memories.id, so a plain string round-trip is safe here.
func (s *Store) fetchRunScopes(ctx context.Context, ids []string) (map[string]rawScope, error) {
	rows, err := s.pgdb.Query(ctx,
		`SELECT id, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon FROM cortex_runs WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch parent run scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]rawScope, len(ids))
	for rows.Next() {
		var (
			id        string
			rs        rawScope
			extraJSON []byte
		)
		if scanErr := rows.Scan(&id, &rs.L0, &rs.L1, &rs.L2, &extraJSON, &rs.Canon); scanErr != nil {
			return nil, fmt.Errorf("scan run scope: %w", scanErr)
		}
		if len(extraJSON) > 0 {
			if uErr := json.Unmarshal(extraJSON, &rs.Extra); uErr != nil {
				return nil, fmt.Errorf("unmarshal scope_extra for run %s: %w", id, uErr)
			}
		}
		if rs.Extra == nil {
			rs.Extra = make(map[string]string)
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

// detectCollisions finds rows that would violate a (scope_canon, name) or
// working-memory (scope_canon, agent_id, kind, key) uniqueness
// expectation once written, and reports them before anything is written.
// Letting a later unique index reject them mid-write would be a
// data-loss surprise; this turns it into a fix-your-rescoper message.
//
// This checks two things, not one: rows within THIS batch colliding with
// each other, and rows in this batch colliding with rows that are
// already scoped (from an earlier pass, or written post-v1.8.0 under a
// real scope). The latter needs a database round trip -- loadExisting*
// pre-seeds `seen`/`seenWorking` with what's already there for the
// canons this batch is about to write, before the in-batch rows are
// checked against them.
func (s *Store) detectCollisions(ctx context.Context, rows []legacyRow, resolved map[string]rawScope) error {
	seen := make(map[[3]string]any)        // table+canon+name -> row id (existing or in-batch)
	seenWorking := make(map[[4]string]any) // canon+agent_id+kind+key -> row id

	if err := s.loadExistingNameCollisions(ctx, rows, resolved, seen); err != nil {
		return err
	}
	if err := s.loadExistingWorkingMemoryCollisions(ctx, rows, resolved, seenWorking); err != nil {
		return err
	}

	for _, r := range rows {
		canon := resolved[r.key()].Canon

		if r.Name != "" {
			k := [3]string{r.Table, canon, r.Name}
			if first, ok := seen[k]; ok {
				return fmt.Errorf(
					"rescope collision: %s rows %v and %v both map to scope %q with name %q; "+
						"the rescoper must keep them in distinct scopes",
					r.Table, first, r.ID, canon, r.Name)
			}
			seen[k] = r.ID
		}

		if r.Table == "cortex_memories" && r.Kind == "working" {
			k := [4]string{canon, r.AgentID, r.Kind, r.Key}
			if first, ok := seenWorking[k]; ok {
				return fmt.Errorf(
					"rescope collision: cortex_memories rows %v and %v both map to scope %q "+
						"with working-memory key (agent=%q key=%q); the rescoper must keep them in distinct scopes",
					first, r.ID, canon, r.AgentID, r.Key)
			}
			seenWorking[k] = r.ID
		}
	}
	return nil
}

// loadExistingNameCollisions pre-seeds seen with every already-scoped row
// (in a name-bearing table) whose scope_canon matches one of the canons
// this batch is about to write. Without this, a legacy row could be
// rescoped onto a scope an existing row already occupies under the same
// name, and the pass would only discover it as a raw unique-violation
// once applyScopes hits the write -- rolled back, but with no indication
// of which row it collided with.
func (s *Store) loadExistingNameCollisions(ctx context.Context, rows []legacyRow, resolved map[string]rawScope, seen map[[3]string]any) error {
	canonsByTable := make(map[string]map[string]struct{})
	for _, r := range rows {
		if r.Name == "" {
			continue
		}
		canon := resolved[r.key()].Canon
		if canonsByTable[r.Table] == nil {
			canonsByTable[r.Table] = make(map[string]struct{})
		}
		canonsByTable[r.Table][canon] = struct{}{}
	}

	for table, canonSet := range canonsByTable {
		canons := make([]string, 0, len(canonSet))
		for c := range canonSet {
			canons = append(canons, c)
		}

		if err := s.loadExistingNamesInTable(ctx, table, canons, seen); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadExistingNamesInTable(ctx context.Context, table string, canons []string, seen map[[3]string]any) error {
	query := `SELECT id, name, scope_canon FROM ` + table + ` WHERE scope_canon = ANY($1)`
	existing, err := s.pgdb.Query(ctx, query, canons)
	if err != nil {
		return fmt.Errorf("check existing names in %s: %w", table, err)
	}
	defer func() { _ = existing.Close() }()

	for existing.Next() {
		var (
			id    any
			name  string
			canon string
		)
		if scanErr := existing.Scan(&id, &name, &canon); scanErr != nil {
			return fmt.Errorf("scan existing name in %s: %w", table, scanErr)
		}
		seen[[3]string{table, canon, name}] = id
	}
	return existing.Err()
}

// loadExistingWorkingMemoryCollisions is loadExistingNameCollisions'
// counterpart for cortex_memories, which has no `name` column and so
// never participates in the name-based check. Its uniqueness surface is
// (agent_id, kind, key, scope_canon) instead -- the same composite the
// scope-aware partial unique index enforces (idx_cortex_memories_working
// in migrations.go).
func (s *Store) loadExistingWorkingMemoryCollisions(ctx context.Context, rows []legacyRow, resolved map[string]rawScope, seen map[[4]string]any) error {
	canonSet := make(map[string]struct{})
	for _, r := range rows {
		if r.Table == "cortex_memories" && r.Kind == "working" {
			canonSet[resolved[r.key()].Canon] = struct{}{}
		}
	}
	if len(canonSet) == 0 {
		return nil
	}

	canons := make([]string, 0, len(canonSet))
	for c := range canonSet {
		canons = append(canons, c)
	}

	existing, err := s.pgdb.Query(ctx,
		`SELECT id, agent_id, "key", scope_canon FROM cortex_memories WHERE kind = 'working' AND scope_canon = ANY($1)`, canons)
	if err != nil {
		return fmt.Errorf("check existing working-memory keys: %w", err)
	}
	defer func() { _ = existing.Close() }()

	for existing.Next() {
		var (
			id      any
			agentID string
			key     string
			canon   string
		)
		if scanErr := existing.Scan(&id, &agentID, &key, &canon); scanErr != nil {
			return fmt.Errorf("scan existing working-memory key: %w", scanErr)
		}
		seen[[4]string{canon, agentID, "working", key}] = id
	}
	return existing.Err()
}

// applyScopes writes every resolved scope inside one transaction, so a
// mid-write failure rolls back whole rather than leaving some rows
// rescoped and others not.
func (s *Store) applyScopes(ctx context.Context, rows []legacyRow, resolved map[string]rawScope) error {
	tx, err := s.pgdb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rescope transaction: %w", err)
	}
	// After a successful Commit, Rollback is a documented no-op; before
	// one, its error can't be acted on any further than the failure that
	// triggered this defer already is.
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // no-op after commit, unactionable otherwise

	for _, r := range rows {
		rs := resolved[r.key()]
		extra := rs.Extra
		if extra == nil {
			extra = make(map[string]string)
		}
		extraJSON, mErr := json.Marshal(extra)
		if mErr != nil {
			return fmt.Errorf("marshal scope_extra for %s row %v: %w", r.Table, r.ID, mErr)
		}
		query := `UPDATE ` + r.Table +
			` SET scope_l0 = $1, scope_l1 = $2, scope_l2 = $3, scope_extra = $4::jsonb, scope_canon = $5 WHERE id = $6`
		if _, execErr := tx.Exec(ctx, query, rs.L0, rs.L1, rs.L2, string(extraJSON), rs.Canon, r.ID); execErr != nil {
			return fmt.Errorf("rescope %s row %v: %w", r.Table, r.ID, execErr)
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

// hasDirectRows reports whether any row in rows needs to be resolved
// directly through the Rescoper, as opposed to inheriting a parent's
// scope. An unscoped run is a direct row itself, so this still requires
// a rescoper whenever a dependent's own parent is unscoped.
func hasDirectRows(rows []legacyRow) bool {
	for _, r := range rows {
		if !r.Dependent {
			return true
		}
	}
	return false
}
