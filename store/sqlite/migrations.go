package sqlite

import (
	"context"
	"fmt"

	"github.com/xraph/grove/migrate"
)

// Migrations is the grove migration group for the Cortex SQLite store.
var Migrations = migrate.NewGroup("cortex")

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
				// and its ADD COLUMN doesn't support IF NOT EXISTS, so each
				// column is its own statement. Mirrors the postgres
				// migration's columns and index, minus the JSONB type
				// (scope_extra is a JSON-encoded TEXT column here).
				//
				// cortex_orchestration_runs is deliberately excluded: see the
				// postgres migration of the same version for why.
				for _, table := range []string{
					"cortex_agents",
					"cortex_runs",
					"cortex_memories",
					"cortex_checkpoints",
				} {
					if _, err := exec.Exec(ctx, `
ALTER TABLE `+table+` ADD COLUMN scope_l0    TEXT NOT NULL DEFAULT '';
ALTER TABLE `+table+` ADD COLUMN scope_l1    TEXT NOT NULL DEFAULT '';
ALTER TABLE `+table+` ADD COLUMN scope_l2    TEXT NOT NULL DEFAULT '';
ALTER TABLE `+table+` ADD COLUMN scope_extra TEXT NOT NULL DEFAULT '{}';
ALTER TABLE `+table+` ADD COLUMN scope_canon TEXT NOT NULL DEFAULT '';
`); err != nil {
						return fmt.Errorf("add scope columns to %s: %w", table, err)
					}
					if _, err := exec.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_`+table+`_scope ON `+table+` (scope_l0, scope_l1, scope_l2);
`); err != nil {
						return fmt.Errorf("index scope on %s: %w", table, err)
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
					if _, err := exec.Exec(ctx, `
ALTER TABLE `+table+` ADD COLUMN scope_l0    TEXT NOT NULL DEFAULT '';
ALTER TABLE `+table+` ADD COLUMN scope_l1    TEXT NOT NULL DEFAULT '';
ALTER TABLE `+table+` ADD COLUMN scope_l2    TEXT NOT NULL DEFAULT '';
ALTER TABLE `+table+` ADD COLUMN scope_extra TEXT NOT NULL DEFAULT '{}';
ALTER TABLE `+table+` ADD COLUMN scope_canon TEXT NOT NULL DEFAULT '';
`); err != nil {
						return fmt.Errorf("add scope columns to %s: %w", table, err)
					}
					if _, err := exec.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_`+table+`_scope ON `+table+` (scope_l0, scope_l1, scope_l2);
`); err != nil {
						return fmt.Errorf("index scope on %s: %w", table, err)
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
	)
}
