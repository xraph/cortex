package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xraph/grove/migrate"

	"github.com/xraph/cortex/id"
)

// Migrations is the grove migration group for the Cortex SQLite store.
var Migrations = migrate.NewGroup("cortex")

// scopeColumnDefs are the host-defined scope columns added to every
// scope-guarded table by the 20260821000001 and 20260822000001 migrations.
var scopeColumnDefs = []struct {
	name string
	ddl  string
}{
	{"scope_l0", "TEXT NOT NULL DEFAULT ''"},
	{"scope_l1", "TEXT NOT NULL DEFAULT ''"},
	{"scope_l2", "TEXT NOT NULL DEFAULT ''"},
	{"scope_extra", "TEXT NOT NULL DEFAULT '{}'"},
	{"scope_canon", "TEXT NOT NULL DEFAULT ''"},
}

// existingColumns reports the columns already present on table, via PRAGMA
// table_info. SQLite doesn't accept bound parameters for PRAGMA table names,
// but table is always one of our own hardcoded constants, never caller
// input.
func existingColumns(ctx context.Context, exec migrate.Executor, table string) (map[string]bool, error) {
	rows, err := exec.Query(ctx, `PRAGMA table_info(`+table+`)`)
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
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return nil, fmt.Errorf("scan columns of %s: %w", table, err)
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// addScopeColumns adds the scope_* columns and their index to table,
// skipping any column that already exists. SQLite's ADD COLUMN has no IF
// NOT EXISTS, so a migration that fails partway through (e.g. after adding
// scope_l0 but before the version is recorded) would abort every rerun on
// "duplicate column name" without this check.
func addScopeColumns(ctx context.Context, exec migrate.Executor, table string) error {
	existing, err := existingColumns(ctx, exec, table)
	if err != nil {
		return err
	}

	for _, col := range scopeColumnDefs {
		if existing[col.name] {
			continue
		}
		if _, err := exec.Exec(ctx, `ALTER TABLE `+table+` ADD COLUMN `+col.name+` `+col.ddl); err != nil {
			return fmt.Errorf("add column %s to %s: %w", col.name, table, err)
		}
	}

	if _, err := exec.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_`+table+`_scope ON `+table+` (scope_l0, scope_l1, scope_l2)`); err != nil {
		return fmt.Errorf("index scope on %s: %w", table, err)
	}
	return nil
}

func init() {
	Migrations.MustRegister(
		&migrate.Migration{
			Name:    "create_agents",
			Version: "20240101000001",
			Comment: "Create cortex_agents table",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_agents (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    app_id           TEXT NOT NULL,
    system_prompt    TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT '',
    tools            TEXT NOT NULL DEFAULT '[]',
    max_steps        INTEGER NOT NULL DEFAULT 0,
    max_tokens       INTEGER NOT NULL DEFAULT 0,
    temperature      REAL NOT NULL DEFAULT 0,
    reasoning_loop   TEXT NOT NULL DEFAULT '',
    guardrails       TEXT NOT NULL DEFAULT '{}',
    metadata         TEXT NOT NULL DEFAULT '{}',
    enabled          INTEGER NOT NULL DEFAULT 1,
    persona_ref      TEXT NOT NULL DEFAULT '',
    inline_skills    TEXT NOT NULL DEFAULT '[]',
    inline_traits    TEXT NOT NULL DEFAULT '[]',
    inline_behaviors TEXT NOT NULL DEFAULT '[]',
    created_at       TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_agents_app_name ON cortex_agents (app_id, name);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_agents`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "create_runs",
			Version: "20240101000002",
			Comment: "Create cortex_runs table",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_runs (
    id            TEXT PRIMARY KEY,
    agent_id      TEXT NOT NULL DEFAULT '',
    tenant_id     TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'created',
    input         TEXT NOT NULL DEFAULT '',
    output        TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT '',
    step_count    INTEGER NOT NULL DEFAULT 0,
    tokens_used   INTEGER NOT NULL DEFAULT 0,
    started_at    TEXT,
    completed_at  TEXT,
    persona_ref   TEXT NOT NULL DEFAULT '',
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_runs_agent ON cortex_runs (agent_id);
CREATE INDEX IF NOT EXISTS idx_cortex_runs_state ON cortex_runs (state);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_runs`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "create_steps",
			Version: "20240101000003",
			Comment: "Create cortex_steps table",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_steps (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL DEFAULT '',
    "index"       INTEGER NOT NULL,
    type          TEXT NOT NULL DEFAULT '',
    input         TEXT NOT NULL DEFAULT '',
    output        TEXT NOT NULL DEFAULT '',
    tokens_used   INTEGER NOT NULL DEFAULT 0,
    started_at    TEXT,
    completed_at  TEXT,
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_steps_run ON cortex_steps (run_id);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_steps`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "create_tool_calls",
			Version: "20240101000004",
			Comment: "Create cortex_tool_calls table",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_tool_calls (
    id            TEXT PRIMARY KEY,
    step_id       TEXT NOT NULL DEFAULT '',
    run_id        TEXT NOT NULL DEFAULT '',
    tool_name     TEXT NOT NULL,
    arguments     TEXT NOT NULL DEFAULT '',
    result        TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT '',
    started_at    TEXT,
    completed_at  TEXT,
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_tool_calls_step ON cortex_tool_calls (step_id);
CREATE INDEX IF NOT EXISTS idx_cortex_tool_calls_run ON cortex_tool_calls (run_id);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_tool_calls`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "create_memories",
			Version: "20240101000005",
			Comment: "Create cortex_memories table",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_memories (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id    TEXT NOT NULL,
    tenant_id   TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL,
    key         TEXT NOT NULL DEFAULT '',
    content     TEXT NOT NULL,
    metadata    TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_memories_agent_kind ON cortex_memories (agent_id, kind);
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_memories_working ON cortex_memories (agent_id, kind, key) WHERE kind = 'working';
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_memories`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "create_checkpoints",
			Version: "20240101000006",
			Comment: "Create cortex_checkpoints table",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_checkpoints (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL DEFAULT '',
    agent_id    TEXT NOT NULL DEFAULT '',
    tenant_id   TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    step_index  INTEGER NOT NULL DEFAULT 0,
    state       TEXT NOT NULL DEFAULT 'pending',
    decision    TEXT,
    metadata    TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_checkpoints_run ON cortex_checkpoints (run_id);
CREATE INDEX IF NOT EXISTS idx_cortex_checkpoints_state ON cortex_checkpoints (state);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_checkpoints`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "create_skills_traits",
			Version: "20240101000007",
			Comment: "Create cortex_skills and cortex_traits tables",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_skills (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL,
    description             TEXT NOT NULL DEFAULT '',
    app_id                  TEXT NOT NULL,
    tools                   TEXT NOT NULL DEFAULT '[]',
    knowledge               TEXT NOT NULL DEFAULT '[]',
    system_prompt_fragment  TEXT NOT NULL DEFAULT '',
    dependencies            TEXT NOT NULL DEFAULT '[]',
    default_proficiency     TEXT NOT NULL DEFAULT '',
    metadata                TEXT NOT NULL DEFAULT '{}',
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at              TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_skills_app_name ON cortex_skills (app_id, name);

CREATE TABLE IF NOT EXISTS cortex_traits (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    app_id      TEXT NOT NULL,
    dimensions  TEXT NOT NULL DEFAULT '[]',
    influences  TEXT NOT NULL DEFAULT '[]',
    category    TEXT NOT NULL DEFAULT '',
    metadata    TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_traits_app_name ON cortex_traits (app_id, name);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP TABLE IF EXISTS cortex_traits;
DROP TABLE IF EXISTS cortex_skills;
`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "create_behaviors_personas",
			Version: "20240101000008",
			Comment: "Create cortex_behaviors and cortex_personas tables",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_behaviors (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    app_id          TEXT NOT NULL,
    triggers        TEXT NOT NULL DEFAULT '[]',
    actions         TEXT NOT NULL DEFAULT '[]',
    priority        INTEGER NOT NULL DEFAULT 0,
    requires_skill  TEXT NOT NULL DEFAULT '',
    requires_trait  TEXT NOT NULL DEFAULT '',
    metadata        TEXT NOT NULL DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_behaviors_app_name ON cortex_behaviors (app_id, name);

CREATE TABLE IF NOT EXISTS cortex_personas (
    id                   TEXT PRIMARY KEY,
    name                 TEXT NOT NULL,
    description          TEXT NOT NULL DEFAULT '',
    app_id               TEXT NOT NULL,
    identity             TEXT NOT NULL DEFAULT '',
    skills               TEXT NOT NULL DEFAULT '[]',
    traits               TEXT NOT NULL DEFAULT '[]',
    behaviors            TEXT NOT NULL DEFAULT '[]',
    cognitive_style      TEXT NOT NULL DEFAULT '{}',
    communication_style  TEXT NOT NULL DEFAULT '{}',
    perception           TEXT NOT NULL DEFAULT '{}',
    metadata             TEXT NOT NULL DEFAULT '{}',
    created_at           TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at           TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_personas_app_name ON cortex_personas (app_id, name);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP TABLE IF EXISTS cortex_personas;
DROP TABLE IF EXISTS cortex_behaviors;
`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "create_orchestrations",
			Version: "20240101000009",
			Comment: "Create cortex_orchestration_configs and cortex_orchestration_runs tables",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_orchestration_configs (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    app_id        TEXT NOT NULL,
    strategy      TEXT NOT NULL DEFAULT '',
    participants  TEXT NOT NULL DEFAULT '[]',
    settings      TEXT NOT NULL DEFAULT '{}',
    metadata      TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_orchestration_configs_app_name ON cortex_orchestration_configs (app_id, name);

CREATE TABLE IF NOT EXISTS cortex_orchestration_runs (
    id             TEXT PRIMARY KEY,
    config_id      TEXT NOT NULL DEFAULT '',
    app_id         TEXT NOT NULL,
    tenant_id      TEXT NOT NULL DEFAULT '',
    strategy       TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'running',
    input          TEXT NOT NULL DEFAULT '',
    output         TEXT NOT NULL DEFAULT '',
    error          TEXT NOT NULL DEFAULT '',
    agent_run_ids  TEXT NOT NULL DEFAULT '[]',
    started_at     TEXT,
    completed_at   TEXT,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_orchestration_runs_app_status ON cortex_orchestration_runs (app_id, status);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP TABLE IF EXISTS cortex_orchestration_runs;
DROP TABLE IF EXISTS cortex_orchestration_configs;
`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "add_scope_columns",
			Version: "20260821000001",
			Comment: "Add host-defined scope columns; pre-v2 rows are left unscoped",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// SQLite only allows one column per ALTER TABLE statement,
				// and its ADD COLUMN doesn't support IF NOT EXISTS, so
				// addScopeColumns checks PRAGMA table_info first and skips
				// columns that already exist -- otherwise a migration that
				// failed partway through would abort every rerun on
				// "duplicate column name". Mirrors the postgres migration's
				// columns and index, minus the JSONB type (scope_extra is a
				// JSON-encoded TEXT column here).
				//
				// cortex_orchestration_configs/cortex_orchestration_runs are
				// deliberately excluded: see the postgres migration of the
				// same version, and 20260823000004 (the migration that
				// finally brings orchestration under scope), for why.
				for _, table := range []string{
					"cortex_agents",
					"cortex_runs",
					"cortex_memories",
					"cortex_checkpoints",
				} {
					if err := addScopeColumns(ctx, exec, table); err != nil {
						return err
					}
				}

				// No backfill. See the postgres migration of the same
				// version for why: inventing "tenant=" / "app=" values for
				// pre-v2 rows would make scope_l0 polysemous per row and
				// silently orphan all v1 conversation history. Pre-existing
				// rows are left with empty scope columns, which every scoped
				// query already treats as non-matching.
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				// SQLite has supported DROP COLUMN since 3.35.0, one at a
				// time like ADD COLUMN.
				for _, table := range []string{
					"cortex_agents",
					"cortex_runs",
					"cortex_memories",
					"cortex_checkpoints",
				} {
					if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_`+table+`_scope;
ALTER TABLE `+table+` DROP COLUMN scope_l0;
ALTER TABLE `+table+` DROP COLUMN scope_l1;
ALTER TABLE `+table+` DROP COLUMN scope_l2;
ALTER TABLE `+table+` DROP COLUMN scope_extra;
ALTER TABLE `+table+` DROP COLUMN scope_canon;
`); err != nil {
						return fmt.Errorf("drop scope columns from %s: %w", table, err)
					}
				}
				return nil
			},
		},
		&migrate.Migration{
			Name:    "add_scope_columns_steps_tool_calls",
			Version: "20260822000001",
			Comment: "Add host-defined scope columns to cortex_steps and cortex_tool_calls; pre-existing rows are left unscoped",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// Split from the 20260821000001 migration: steps and tool
				// calls held the same verbatim LLM input/output as
				// conversation memory but were never brought under scope
				// guard, so isolation depended on every reader reaching
				// them through a scoped GetRun first instead of the store
				// enforcing it directly. Same column shape and index as
				// every other scoped table.
				for _, table := range []string{
					"cortex_steps",
					"cortex_tool_calls",
				} {
					if err := addScopeColumns(ctx, exec, table); err != nil {
						return err
					}
				}

				// No backfill, same reasoning as 20260821000001: a
				// fabricated level for pre-existing rows would make
				// scope_l0 polysemous per row and silently orphan old
				// history instead of leaving it correctly invisible to
				// every scoped query.
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				for _, table := range []string{
					"cortex_steps",
					"cortex_tool_calls",
				} {
					if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_`+table+`_scope;
ALTER TABLE `+table+` DROP COLUMN scope_l0;
ALTER TABLE `+table+` DROP COLUMN scope_l1;
ALTER TABLE `+table+` DROP COLUMN scope_l2;
ALTER TABLE `+table+` DROP COLUMN scope_extra;
ALTER TABLE `+table+` DROP COLUMN scope_canon;
`); err != nil {
						return fmt.Errorf("drop scope columns from %s: %w", table, err)
					}
				}
				return nil
			},
		},
		&migrate.Migration{
			Name:    "scope_working_memory_unique_index",
			Version: "20260822000002",
			Comment: "Make the working-memory partial unique index scope-aware",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// idx_cortex_memories_working used to be (agent_id, kind,
				// key) WHERE kind = 'working', with no scope column at
				// all. That let a caller in scope B upsert a working-memory
				// row using a run ID it only knew about (a run ID is a
				// bearer capability, not an isolation boundary) and hit the
				// SAME conflict target as scope A's row: the DO UPDATE
				// overwrote A's content while leaving A's own scope columns
				// in place, so A's next scoped LoadWorking returned B's
				// value. Adding scope_canon to the index means two
				// different scopes can never conflict with each other in
				// the first place.
				_, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_memories_working;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_memories_working ON cortex_memories (agent_id, kind, key, scope_canon) WHERE kind = 'working';
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_memories_working;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_memories_working ON cortex_memories (agent_id, kind, key) WHERE kind = 'working';
`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "scope_agents_unique_index",
			Version: "20260823000001",
			Comment: "Replace the app_id-keyed agent name uniqueness with a scope-keyed one",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// idx_cortex_agents_app_name enforced uniqueness on
				// (app_id, name), from before agents carried a scope at
				// all. Now that Create/Get/GetByName/Update/Delete/List
				// are all scope-guarded, two different scopes must be
				// able to each use the name "assistant" — app_id was
				// never the isolation boundary here, scope is, so the
				// index has to key on scope_canon instead or the second
				// scope's Create collides on the first scope's row before
				// the scope predicate ever gets a chance to matter.
				//
				// Partial (WHERE scope_canon != '') rather than a plain
				// unique index: this migration runs before
				// rescopeLegacyRows, so any pre-v1.8.0 rows are still
				// sitting at scope_canon = '' when the index is built.
				// Two such rows sharing a name but never colliding under
				// the old app_id-keyed index (different apps, say) would
				// make a non-partial CREATE UNIQUE INDEX fail outright on
				// existing data before the rescoper ever got a chance to
				// separate them. Every row Create writes always carries a
				// real, non-empty scope_canon (Create rejects a zero
				// scope), so the partial index gives every current and
				// future write the same protection a full index would —
				// it only widens the exemption for not-yet-rescoped
				// legacy rows, which stay invisible to every scoped query
				// anyway per the 20260821000001 migration's reasoning.
				_, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_agents_app_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_agents_scope_name ON cortex_agents (scope_canon, name) WHERE scope_canon != '';
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				// This recreates UNIQUE (app_id, name), and every row
				// written since v1.8.0 has app_id = ''. If any two scopes
				// hold an agent with the same name, that collision makes
				// this CREATE UNIQUE INDEX fail outright — rollback is
				// only available if no such pair exists yet.
				_, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_agents_scope_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_agents_app_name ON cortex_agents (app_id, name);
`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "scope_skills_traits_behaviors",
			Version: "20260823000002",
			Comment: "Add host-defined scope columns to cortex_skills/cortex_traits/cortex_behaviors and replace their app_id-keyed unique indexes with scope-keyed ones",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// Same column shape and index as every other scoped table
				// (see 20260821000001); addScopeColumns already skips
				// columns that exist, so this is idempotent the same way.
				// No backfill, same reasoning: a fabricated level for
				// pre-existing rows would make scope_l0 polysemous per
				// row and silently orphan old rows instead of leaving
				// them correctly invisible to every scoped query until
				// rescopeLegacyRows resolves them.
				for _, table := range []string{
					"cortex_skills",
					"cortex_traits",
					"cortex_behaviors",
				} {
					if err := addScopeColumns(ctx, exec, table); err != nil {
						return err
					}
				}

				// idx_cortex_{skills,traits,behaviors}_app_name enforced
				// uniqueness on (app_id, name), from before these three
				// carried a scope at all. Now that every method on all
				// three stores is scope-guarded, two different scopes
				// must be able to each use the same name — app_id was
				// never the isolation boundary here, scope is, so the
				// index has to key on scope_canon instead or the second
				// scope's Create collides on the first scope's row
				// before the scope predicate ever gets a chance to
				// matter.
				//
				// Partial (WHERE scope_canon != '') rather than a plain
				// unique index, for the exact reason 20260823000001
				// documents for cortex_agents: this migration runs
				// before rescopeLegacyRows, so any pre-v1.8.0 rows are
				// still sitting at scope_canon = '' when the index is
				// built, and a plain unique index would make every such
				// row collide on ('', name) instead of letting the
				// rescoper separate them first.
				for _, spec := range []struct{ table, oldIdx, newIdx string }{
					{"cortex_skills", "idx_cortex_skills_app_name", "idx_cortex_skills_scope_name"},
					{"cortex_traits", "idx_cortex_traits_app_name", "idx_cortex_traits_scope_name"},
					{"cortex_behaviors", "idx_cortex_behaviors_app_name", "idx_cortex_behaviors_scope_name"},
				} {
					if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS `+spec.oldIdx+`;
CREATE UNIQUE INDEX IF NOT EXISTS `+spec.newIdx+` ON `+spec.table+` (scope_canon, name) WHERE scope_canon != '';
`); err != nil {
						return fmt.Errorf("scope-key unique index on %s: %w", spec.table, err)
					}
				}
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				// Each loop iteration recreates UNIQUE (app_id, name) on
				// its table, and every row written since v1.8.0 has
				// app_id = ''. If any two scopes hold a skill, trait, or
				// behavior with the same name, the CREATE UNIQUE INDEX
				// for that table fails outright — rollback is only
				// available if no such pair exists yet.
				for _, spec := range []struct{ table, oldIdx, newIdx string }{
					{"cortex_skills", "idx_cortex_skills_app_name", "idx_cortex_skills_scope_name"},
					{"cortex_traits", "idx_cortex_traits_app_name", "idx_cortex_traits_scope_name"},
					{"cortex_behaviors", "idx_cortex_behaviors_app_name", "idx_cortex_behaviors_scope_name"},
				} {
					if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS `+spec.newIdx+`;
CREATE UNIQUE INDEX IF NOT EXISTS `+spec.oldIdx+` ON `+spec.table+` (app_id, name);
`); err != nil {
						return fmt.Errorf("restore app_id-key unique index on %s: %w", spec.table, err)
					}
				}
				for _, table := range []string{
					"cortex_skills",
					"cortex_traits",
					"cortex_behaviors",
				} {
					if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_`+table+`_scope;
ALTER TABLE `+table+` DROP COLUMN scope_l0;
ALTER TABLE `+table+` DROP COLUMN scope_l1;
ALTER TABLE `+table+` DROP COLUMN scope_l2;
ALTER TABLE `+table+` DROP COLUMN scope_extra;
ALTER TABLE `+table+` DROP COLUMN scope_canon;
`); err != nil {
						return fmt.Errorf("drop scope columns from %s: %w", table, err)
					}
				}
				return nil
			},
		},
		&migrate.Migration{
			Name:    "scope_personas",
			Version: "20260823000003",
			Comment: "Add host-defined scope columns to cortex_personas and replace its app_id-keyed unique index with a scope-keyed one",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// Same column shape and index as every other scoped table
				// (see 20260821000001); addScopeColumns already skips
				// columns that exist, so this is idempotent the same way.
				// No backfill, same reasoning: a fabricated level for
				// pre-existing rows would make scope_l0 polysemous per
				// row and silently orphan old rows instead of leaving
				// them correctly invisible to every scoped query until
				// rescopeLegacyRows resolves them.
				if err := addScopeColumns(ctx, exec, "cortex_personas"); err != nil {
					return err
				}

				// idx_cortex_personas_app_name enforced uniqueness on
				// (app_id, name), from before personas carried a scope at
				// all. Now that every method on the persona store is
				// scope-guarded, two different scopes must be able to
				// each use the same name — app_id was never the isolation
				// boundary here, scope is, so the index has to key on
				// scope_canon instead or the second scope's Create
				// collides on the first scope's row before the scope
				// predicate ever gets a chance to matter.
				//
				// Partial (WHERE scope_canon != '') rather than a plain
				// unique index, for the exact reason 20260823000001
				// documents for cortex_agents: this migration runs
				// before rescopeLegacyRows, so any pre-v1.8.0 rows are
				// still sitting at scope_canon = '' when the index is
				// built, and a plain unique index would make every such
				// row collide on ('', name) instead of letting the
				// rescoper separate them first.
				if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_personas_app_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_personas_scope_name ON cortex_personas (scope_canon, name) WHERE scope_canon != '';
`); err != nil {
					return fmt.Errorf("scope-key unique index on cortex_personas: %w", err)
				}
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				// This recreates UNIQUE (app_id, name), and every row
				// written since v1.8.0 has app_id = ''. If any two scopes
				// hold a persona with the same name, that collision makes
				// this CREATE UNIQUE INDEX fail outright — rollback is
				// only available if no such pair exists yet.
				if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_personas_scope_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_personas_app_name ON cortex_personas (app_id, name);
`); err != nil {
					return fmt.Errorf("restore app_id-key unique index on cortex_personas: %w", err)
				}
				if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_personas_scope;
ALTER TABLE cortex_personas DROP COLUMN scope_l0;
ALTER TABLE cortex_personas DROP COLUMN scope_l1;
ALTER TABLE cortex_personas DROP COLUMN scope_l2;
ALTER TABLE cortex_personas DROP COLUMN scope_extra;
ALTER TABLE cortex_personas DROP COLUMN scope_canon;
`); err != nil {
					return fmt.Errorf("drop scope columns from cortex_personas: %w", err)
				}
				return nil
			},
		},
		&migrate.Migration{
			Name:    "scope_orchestrations",
			Version: "20260823000004",
			Comment: "Add host-defined scope columns to cortex_orchestration_configs/cortex_orchestration_runs and replace the configs' app_id-keyed unique index with a scope-keyed one",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// Orchestration is the last entity converted in this
				// phase, and the only one that had its scope columns
				// dropped once already (see 20260821000001's comment):
				// the earlier attempt populated only ScopeExtra and left
				// scope_l0/l1/l2/canon empty, which read as coverage that
				// wasn't there. This time
				// orchestrationConfigToModel/orchestrationRunToModel
				// write all five columns via scopeColumns, the same as
				// every other scoped table. addScopeColumns already
				// skips columns that exist, so this is idempotent the
				// same way.
				for _, table := range []string{
					"cortex_orchestration_configs",
					"cortex_orchestration_runs",
				} {
					if err := addScopeColumns(ctx, exec, table); err != nil {
						return err
					}
				}

				// idx_cortex_orchestration_configs_app_name enforced
				// uniqueness on (app_id, name), from before orchestration
				// configs carried a scope at all. Now that every method
				// on ConfigStore is scope-guarded, two different scopes
				// must be able to each use the same name — app_id was
				// never the isolation boundary, scope is, so the unique
				// index has to key on scope_canon instead or the second
				// scope's Create collides on the first scope's row
				// before the scope predicate ever gets a chance to
				// matter.
				//
				// The app_id column itself is not dropped here, but not
				// because any lookup still needs it:
				// GetOrchestrationByName(ctx, name) takes no appID
				// parameter. It stays because rescopeLegacyRows reads it
				// off pre-v1.8.0 rows to reconstruct the scope a
				// Rescoper should assign them.
				//
				// Partial (WHERE scope_canon != '') rather than a plain
				// unique index, for the exact reason 20260823000001
				// documents for cortex_agents: this migration runs
				// before rescopeLegacyRows, so any pre-v1.8.0 rows are
				// still sitting at scope_canon = '' when the index is
				// built, and a plain unique index would make every such
				// row collide on ('', name) instead of letting the
				// rescoper separate them first.
				if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_orchestration_configs_app_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_orchestration_configs_scope_name ON cortex_orchestration_configs (scope_canon, name) WHERE scope_canon != '';
`); err != nil {
					return fmt.Errorf("scope-key unique index on cortex_orchestration_configs: %w", err)
				}
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				// This recreates UNIQUE (app_id, name), and every row
				// written since v1.8.0 has app_id = ''. If any two scopes
				// hold an orchestration config with the same name, that
				// collision makes this CREATE UNIQUE INDEX fail outright
				// — rollback is only available if no such pair exists
				// yet.
				if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_orchestration_configs_scope_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_orchestration_configs_app_name ON cortex_orchestration_configs (app_id, name);
`); err != nil {
					return fmt.Errorf("restore app_id-key unique index on cortex_orchestration_configs: %w", err)
				}
				for _, table := range []string{
					"cortex_orchestration_configs",
					"cortex_orchestration_runs",
				} {
					if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_`+table+`_scope;
ALTER TABLE `+table+` DROP COLUMN scope_l0;
ALTER TABLE `+table+` DROP COLUMN scope_l1;
ALTER TABLE `+table+` DROP COLUMN scope_l2;
ALTER TABLE `+table+` DROP COLUMN scope_extra;
ALTER TABLE `+table+` DROP COLUMN scope_canon;
`); err != nil {
						return fmt.Errorf("drop scope columns from %s: %w", table, err)
					}
				}
				return nil
			},
		},
		&migrate.Migration{
			Name:    "create_sessions",
			Version: "20260824000001",
			Comment: "Create cortex_sessions table, scoped from birth",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// Session is new this phase, so unlike every scoped table
				// before it there is no unscoped legacy shape to carry
				// forward: it is created with its scope columns already in
				// place rather than getting them bolted on by a later
				// migration.
				//
				// backfilled_by is cortex-owned, unlike metadata (the
				// host's column, right above it): it is the migration
				// version that created a session via the v1.9.0 backfill,
				// empty for every session a run creates organically. See
				// backfillSessionMarker and unbackfillDefaultSessions
				// below -- Down finds exactly the rows it's responsible
				// for undoing by this column, and a host PUTting its own
				// metadata can never collide with or erase it, because
				// it never touches metadata.
				if _, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_sessions (
    id            TEXT PRIMARY KEY,
    agent_id      TEXT NOT NULL,
    title         TEXT NOT NULL DEFAULT '',
    message_count INTEGER NOT NULL DEFAULT 0,
    last_message  TEXT NOT NULL DEFAULT '',
    is_default    INTEGER NOT NULL DEFAULT 0,
    metadata      TEXT NOT NULL DEFAULT '{}',
    backfilled_by TEXT NOT NULL DEFAULT '',
    scope_l0      TEXT NOT NULL DEFAULT '',
    scope_l1      TEXT NOT NULL DEFAULT '',
    scope_l2      TEXT NOT NULL DEFAULT '',
    scope_extra   TEXT NOT NULL DEFAULT '{}',
    scope_canon   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_sessions_scope
    ON cortex_sessions (scope_l0, scope_l1, scope_l2);

CREATE INDEX IF NOT EXISTS idx_cortex_sessions_agent
    ON cortex_sessions (agent_id, scope_canon);
`); err != nil {
					return fmt.Errorf("create cortex_sessions: %w", err)
				}

				// Both predicates are load-bearing. is_default keeps
				// non-default sessions out of the constraint, so an agent
				// can hold many threads at once. scope_canon != '' keeps
				// this in step with every other partial unique index in
				// this codebase: Migrate builds indexes before
				// rescopeLegacyRows fills scope columns on other tables,
				// and while cortex_sessions never carries unscoped rows
				// (CreateSession always stamps a real scope), a plain
				// unique index here would still be wrong on its own
				// terms — it would collide the instant two agents in
				// different scopes each got their first default session
				// at scope_canon = '', which can only happen if a future
				// caller ever manages to slip a zero scope through.
				if _, err := exec.Exec(ctx, `
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_sessions_default
    ON cortex_sessions (agent_id, scope_canon)
    WHERE is_default AND scope_canon != '';
`); err != nil {
					return fmt.Errorf("default-session unique index on cortex_sessions: %w", err)
				}
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_sessions`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "add_memory_session_id",
			Version: "20260824000002",
			Comment: "Key conversation memory on a session",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				// SQLite's ADD COLUMN has no IF NOT EXISTS, so check
				// PRAGMA table_info first the same way addScopeColumns
				// does — otherwise a migration that failed partway
				// through would abort every rerun on "duplicate column
				// name".
				existing, err := existingColumns(ctx, exec, "cortex_memories")
				if err != nil {
					return err
				}
				if !existing["session_id"] {
					if _, err := exec.Exec(ctx, `ALTER TABLE cortex_memories ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`); err != nil {
						return fmt.Errorf("add session_id to cortex_memories: %w", err)
					}
				}
				if _, err := exec.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_cortex_memories_session ON cortex_memories (session_id, scope_canon)`); err != nil {
					return fmt.Errorf("index session_id on cortex_memories: %w", err)
				}
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_memories_session;
ALTER TABLE cortex_memories DROP COLUMN session_id;
`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "add_run_session_id",
			Version: "20260824000003",
			Comment: "Carry the resolved session on a run",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				existing, err := existingColumns(ctx, exec, "cortex_runs")
				if err != nil {
					return err
				}
				if existing["session_id"] {
					return nil
				}
				if _, err := exec.Exec(ctx, `ALTER TABLE cortex_runs ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`); err != nil {
					return fmt.Errorf("add session_id to cortex_runs: %w", err)
				}
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `ALTER TABLE cortex_runs DROP COLUMN session_id`)
				return err
			},
		},
		&migrate.Migration{
			Name:    "backfill_default_sessions",
			Version: "20260824000004",
			Comment: "Reserved: the default-session backfill runs unconditionally in Store.Migrate, after rescopeLegacyRows, not as this migration's one-shot Up -- see the Up func's comment",
			// This Up is deliberately a no-op. See the postgres migration
			// of the same version for the full reasoning (identical here):
			// grove runs a migration's Up exactly once per recorded
			// version, inside the migration group, which completes BEFORE
			// rescopeLegacyRows runs (see Store.Migrate in store.go). A
			// host jumping straight from pre-v1.8.0 to this version in one
			// Migrate() call would have every legacy conversation row
			// sitting at scope_canon = '' at the point this Up executed --
			// backfillDefaultSessions' own WHERE scope_canon != '' filter
			// would find nothing, and because grove never retries a
			// recorded version, no later boot would either. Store.Migrate
			// now calls backfillDefaultSessions directly after
			// rescopeLegacyRows, unconditionally on every boot -- safe to
			// run repeatedly since its own filter (session_id = '' on
			// kind = 'conversation') has nothing left to find once a
			// scope's rows have been backfilled once. This migration
			// version stays registered so the schema history and
			// grove_migrations bookkeeping don't lose the record of when
			// this landed.
			Up: func(context.Context, migrate.Executor) error { return nil },
			// Down still finds every session backfillDefaultSessions has
			// ever created, via backfillSessionMarker, and undoes it,
			// regardless of whether it was created by this migration's own
			// Up (it no longer is) or by the unconditional post-rescope
			// call in Store.Migrate. It cannot stop the very next
			// Migrate() call from recreating those sessions -- the
			// underlying legacy rows still have session_id = '' after
			// Down runs, and the backfill call in Store.Migrate has no
			// "already applied" gate to disable.
			Down: unbackfillDefaultSessions,
		},
	)
}

// backfillSessionMarker tags every cortex_sessions row this migration
// creates, in the cortex-owned backfilled_by column, so
// unbackfillDefaultSessions (Down) can find exactly the rows it is
// responsible for undoing without mistaking an organically-created
// default session for one of its own -- engine.resolveSession stamps the
// identical IsDefault=true, Title "Default" shape, but never sets
// backfilled_by. This used to live in Metadata, which session.Session
// documents as belonging to the host: a host that PUT its own metadata
// on a backfilled session would silently destroy the marker Down
// depends on. backfilled_by is a dedicated column cortex alone writes,
// so it survives that.
const backfillSessionMarker = "20260824000004"

// legacyConversationScope is one distinct (agent_id, scope) pairing this
// migration found sitting on unsessioned conversation rows.
type legacyConversationScope struct {
	agentID, l0, l1, l2, canon string
}

// legacyMessage is the subset of memory.Message this migration needs off
// a stored conversation row's JSON content. Role and Content are the only
// two fields that survive llmToMemory's reload-then-resave round trip
// unchanged (see backfillDefaultSessions' comment on the duplication
// bug) -- everything else in memory.Message is deliberately not decoded
// here.
type legacyMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// backfillDefaultSessions does the actual work migration 20260824000004
// was originally going to do as its own Up; it is now called directly
// from Store.Migrate (store.go), AFTER rescopeLegacyRows, unconditionally
// on every boot -- not from the migration's Up, which is a no-op. See
// the postgres migration of the same version for the full reasoning
// behind that move, the scope_canon != "" filter, and the
// distinct-(role,content) message_count -- this is the same logic
// against sqlite's `?` placeholder style.
func backfillDefaultSessions(ctx context.Context, exec migrate.Executor) error {
	scopes, err := findLegacyConversationScopes(ctx, exec)
	if err != nil {
		return fmt.Errorf("find legacy conversation scopes: %w", err)
	}
	for _, sc := range scopes {
		if err := backfillOneScope(ctx, exec, sc); err != nil {
			return fmt.Errorf("backfill default session for agent %s: %w", sc.agentID, err)
		}
	}
	return nil
}

func findLegacyConversationScopes(ctx context.Context, exec migrate.Executor) ([]legacyConversationScope, error) {
	rows, err := exec.Query(ctx, `
SELECT DISTINCT agent_id, scope_l0, scope_l1, scope_l2, scope_canon
  FROM cortex_memories
 WHERE kind = 'conversation' AND session_id = '' AND scope_canon != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []legacyConversationScope
	for rows.Next() {
		var sc legacyConversationScope
		if scanErr := rows.Scan(&sc.agentID, &sc.l0, &sc.l1, &sc.l2, &sc.canon); scanErr != nil {
			return nil, fmt.Errorf("scan legacy conversation scope: %w", scanErr)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// backfillOneScope creates (or reuses) the default session for one
// (agent_id, scope_canon) pair and points that pair's orphaned
// conversation rows at it. See the postgres migration's comment on
// backfillOneScope for why the SELECT after the INSERT is required
// rather than trusting the freshly minted id -- the same lazy-create
// race applies here.
func backfillOneScope(ctx context.Context, exec migrate.Executor, sc legacyConversationScope) error {
	messageCount, lastMessage, err := scanLegacyMessages(ctx, exec, sc)
	if err != nil {
		return fmt.Errorf("read conversation rows: %w", err)
	}

	newID := id.NewSessionID()
	now := time.Now().UTC()
	if _, insErr := exec.Exec(ctx, `
INSERT INTO cortex_sessions
    (id, agent_id, title, message_count, last_message, is_default, backfilled_by,
     scope_l0, scope_l1, scope_l2, scope_extra, scope_canon, created_at, updated_at)
VALUES (?, ?, 'Default', ?, ?, TRUE, ?, ?, ?, ?, '{}', ?, ?, ?)
ON CONFLICT (agent_id, scope_canon) WHERE is_default AND scope_canon != '' DO NOTHING`,
		newID.String(), sc.agentID, messageCount, lastMessage, backfillSessionMarker,
		sc.l0, sc.l1, sc.l2, sc.canon, now, now,
	); insErr != nil {
		return fmt.Errorf("insert default session: %w", insErr)
	}

	sid, err := defaultSessionID(ctx, exec, sc.agentID, sc.canon)
	if err != nil {
		return fmt.Errorf("resolve default session: %w", err)
	}

	if _, updErr := exec.Exec(ctx, `
UPDATE cortex_memories SET session_id = ?
 WHERE kind = 'conversation' AND session_id = '' AND agent_id = ? AND scope_canon = ?`,
		sid, sc.agentID, sc.canon,
	); updErr != nil {
		return fmt.Errorf("point conversation rows at default session: %w", updErr)
	}
	return nil
}

// scanLegacyMessages reads every orphaned conversation row for one
// (agent_id, scope_canon) pair and reduces it to the two values the new
// session needs: a distinct-(role,content) message count and the
// content of the chronologically-last message with non-empty content.
// See the postgres migration's scanLegacyMessages for the full
// reasoning.
func scanLegacyMessages(ctx context.Context, exec migrate.Executor, sc legacyConversationScope) (messageCount int, lastMessage string, err error) {
	rows, err := exec.Query(ctx, `
SELECT content FROM cortex_memories
 WHERE kind = 'conversation' AND session_id = '' AND agent_id = ? AND scope_canon = ?
 ORDER BY created_at ASC`, sc.agentID, sc.canon)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[[2]string]struct{})
	for rows.Next() {
		var content string
		if scanErr := rows.Scan(&content); scanErr != nil {
			return 0, "", fmt.Errorf("scan conversation row: %w", scanErr)
		}
		var msg legacyMessage
		if jsonErr := json.Unmarshal([]byte(content), &msg); jsonErr != nil {
			// A row that isn't valid JSON can't have been written by
			// SaveConversation; skip it from the count rather than
			// aborting the whole backfill over one corrupt row.
			continue
		}
		seen[[2]string{msg.Role, msg.Content}] = struct{}{}
		if msg.Content != "" {
			lastMessage = msg.Content
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	return len(seen), lastMessage, nil
}

// defaultSessionID reads back the id of whichever session currently
// holds the (agent_id, scope_canon) default slot -- the one
// backfillOneScope just inserted, or one that already existed there.
func defaultSessionID(ctx context.Context, exec migrate.Executor, agentID, canon string) (string, error) {
	rows, err := exec.Query(ctx, `
SELECT id FROM cortex_sessions
 WHERE agent_id = ? AND scope_canon = ? AND is_default = TRUE
 LIMIT 1`, agentID, canon)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		return "", fmt.Errorf("no default session found for agent %s in scope %s after insert", agentID, canon)
	}
	var sid string
	if scanErr := rows.Scan(&sid); scanErr != nil {
		return "", fmt.Errorf("scan default session id: %w", scanErr)
	}
	return sid, rows.Err()
}

// unbackfillDefaultSessions is the Down side of this migration. See the
// postgres migration of the same version for the full reasoning; the
// short version is that backfillSessionMarker in the cortex-owned
// backfilled_by column is what makes this safe to run at all -- an
// organically-created default session has the identical IsDefault=true,
// Title "Default" shape but never sets backfilled_by, so this only ever
// touches rows this migration itself wrote. A dedicated column, not
// Metadata: session.Session documents Metadata as the host's, and a
// host PUTting its own metadata on a backfilled session must not be
// able to erase the marker this depends on.
func unbackfillDefaultSessions(ctx context.Context, exec migrate.Executor) error {
	rows, err := exec.Query(ctx, `SELECT id FROM cortex_sessions WHERE backfilled_by = ?`, backfillSessionMarker)
	if err != nil {
		return fmt.Errorf("find backfilled sessions: %w", err)
	}
	var ids []string
	for rows.Next() {
		var sid string
		if scanErr := rows.Scan(&sid); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("scan backfilled session id: %w", scanErr)
		}
		ids = append(ids, sid)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		_ = rows.Close()
		return rowsErr
	}
	_ = rows.Close()

	for _, sid := range ids {
		if _, resetErr := exec.Exec(ctx, `UPDATE cortex_memories SET session_id = '' WHERE session_id = ?`, sid); resetErr != nil {
			return fmt.Errorf("reset session_id for session %s: %w", sid, resetErr)
		}
		if _, delErr := exec.Exec(ctx, `DELETE FROM cortex_sessions WHERE id = ?`, sid); delErr != nil {
			return fmt.Errorf("delete backfilled session %s: %w", sid, delErr)
		}
	}
	return nil
}
