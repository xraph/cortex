package postgres

import (
	"context"
	"fmt"

	"github.com/xraph/grove/migrate"
)

// Migrations is the grove migration group for the Cortex postgres store.
// It contains all schema migrations in version order.
var Migrations = func() *migrate.Group {
	g := migrate.NewGroup("cortex")
	g.MustRegister(
		&migrate.Migration{
			Name:    "create_agents",
			Version: "20240101000001",
			Comment: "Create cortex_agents table",
			Up: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_agents (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    description     TEXT DEFAULT '',
    app_id          TEXT NOT NULL,
    system_prompt   TEXT DEFAULT '',
    model           TEXT DEFAULT '',
    tools           JSONB DEFAULT '[]',
    max_steps       INTEGER DEFAULT 0,
    max_tokens      INTEGER DEFAULT 0,
    temperature     DOUBLE PRECISION DEFAULT 0,
    reasoning_loop  TEXT DEFAULT '',
    guardrails      JSONB DEFAULT '{}',
    metadata        JSONB DEFAULT '{}',
    enabled         BOOLEAN DEFAULT TRUE,
    persona_ref     TEXT DEFAULT '',
    inline_skills   JSONB DEFAULT '[]',
    inline_traits   JSONB DEFAULT '[]',
    inline_behaviors JSONB DEFAULT '[]',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_agents_app_name ON cortex_agents (app_id, name);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_agents CASCADE`)
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
    agent_id      TEXT NOT NULL REFERENCES cortex_agents(id) ON DELETE CASCADE,
    tenant_id     TEXT DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'created',
    input         TEXT DEFAULT '',
    output        TEXT DEFAULT '',
    error         TEXT DEFAULT '',
    step_count    INTEGER DEFAULT 0,
    tokens_used   INTEGER DEFAULT 0,
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    persona_ref   TEXT DEFAULT '',
    metadata      JSONB DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cortex_runs_agent ON cortex_runs (agent_id);
CREATE INDEX IF NOT EXISTS idx_cortex_runs_state ON cortex_runs (state);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_runs CASCADE`)
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
    run_id        TEXT NOT NULL REFERENCES cortex_runs(id) ON DELETE CASCADE,
    "index"       INTEGER NOT NULL,
    type          TEXT DEFAULT '',
    input         TEXT DEFAULT '',
    output        TEXT DEFAULT '',
    tokens_used   INTEGER DEFAULT 0,
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    metadata      JSONB DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cortex_steps_run ON cortex_steps (run_id);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_steps CASCADE`)
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
    step_id       TEXT NOT NULL REFERENCES cortex_steps(id) ON DELETE CASCADE,
    run_id        TEXT NOT NULL REFERENCES cortex_runs(id) ON DELETE CASCADE,
    tool_name     TEXT NOT NULL,
    arguments     TEXT DEFAULT '',
    result        TEXT DEFAULT '',
    error         TEXT DEFAULT '',
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    metadata      JSONB DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cortex_tool_calls_step ON cortex_tool_calls (step_id);
CREATE INDEX IF NOT EXISTS idx_cortex_tool_calls_run ON cortex_tool_calls (run_id);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_tool_calls CASCADE`)
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
    id          BIGSERIAL PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    tenant_id   TEXT DEFAULT '',
    kind        TEXT NOT NULL,
    key         TEXT DEFAULT '',
    content     TEXT NOT NULL,
    metadata    JSONB DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cortex_memories_agent_kind ON cortex_memories (agent_id, kind);
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_memories_working ON cortex_memories (agent_id, kind, key) WHERE kind = 'working';
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_memories CASCADE`)
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
    run_id      TEXT NOT NULL REFERENCES cortex_runs(id) ON DELETE CASCADE,
    agent_id    TEXT NOT NULL REFERENCES cortex_agents(id) ON DELETE CASCADE,
    tenant_id   TEXT DEFAULT '',
    reason      TEXT DEFAULT '',
    step_index  INTEGER DEFAULT 0,
    state       TEXT NOT NULL DEFAULT 'pending',
    decision    JSONB,
    metadata    JSONB DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cortex_checkpoints_run ON cortex_checkpoints (run_id);
CREATE INDEX IF NOT EXISTS idx_cortex_checkpoints_state ON cortex_checkpoints (state);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_checkpoints CASCADE`)
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
    description             TEXT DEFAULT '',
    app_id                  TEXT NOT NULL,
    tools                   JSONB DEFAULT '[]',
    knowledge               JSONB DEFAULT '[]',
    system_prompt_fragment  TEXT DEFAULT '',
    dependencies            JSONB DEFAULT '[]',
    default_proficiency     TEXT DEFAULT '',
    metadata                JSONB DEFAULT '{}',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_skills_app_name ON cortex_skills (app_id, name);

CREATE TABLE IF NOT EXISTS cortex_traits (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT DEFAULT '',
    app_id      TEXT NOT NULL,
    dimensions  JSONB DEFAULT '[]',
    influences  JSONB DEFAULT '[]',
    category    TEXT DEFAULT '',
    metadata    JSONB DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_traits_app_name ON cortex_traits (app_id, name);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP TABLE IF EXISTS cortex_traits CASCADE;
DROP TABLE IF EXISTS cortex_skills CASCADE;
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
    description     TEXT DEFAULT '',
    app_id          TEXT NOT NULL,
    triggers        JSONB DEFAULT '[]',
    actions         JSONB DEFAULT '[]',
    priority        INTEGER DEFAULT 0,
    requires_skill  TEXT DEFAULT '',
    requires_trait  TEXT DEFAULT '',
    metadata        JSONB DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_behaviors_app_name ON cortex_behaviors (app_id, name);

CREATE TABLE IF NOT EXISTS cortex_personas (
    id                   TEXT PRIMARY KEY,
    name                 TEXT NOT NULL,
    description          TEXT DEFAULT '',
    app_id               TEXT NOT NULL,
    identity             TEXT DEFAULT '',
    skills               JSONB DEFAULT '[]',
    traits               JSONB DEFAULT '[]',
    behaviors            JSONB DEFAULT '[]',
    cognitive_style      JSONB DEFAULT '{}',
    communication_style  JSONB DEFAULT '{}',
    perception           JSONB DEFAULT '{}',
    metadata             JSONB DEFAULT '{}',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_personas_app_name ON cortex_personas (app_id, name);
`)
				return err
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				_, err := exec.Exec(ctx, `
DROP TABLE IF EXISTS cortex_personas CASCADE;
DROP TABLE IF EXISTS cortex_behaviors CASCADE;
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
    description   TEXT DEFAULT '',
    app_id        TEXT NOT NULL,
    strategy      TEXT DEFAULT '',
    participants  JSONB DEFAULT '[]',
    settings      JSONB DEFAULT '{}',
    metadata      JSONB DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_orchestration_configs_app_name ON cortex_orchestration_configs (app_id, name);

CREATE TABLE IF NOT EXISTS cortex_orchestration_runs (
    id             TEXT PRIMARY KEY,
    config_id      TEXT DEFAULT '',
    app_id         TEXT NOT NULL,
    tenant_id      TEXT DEFAULT '',
    strategy       TEXT DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'running',
    input          TEXT DEFAULT '',
    output         TEXT DEFAULT '',
    error          TEXT DEFAULT '',
    agent_run_ids  JSONB DEFAULT '[]',
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
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
				// cortex_orchestration_runs is deliberately excluded: orchestration
				// is out of this phase's scope, orchestrationRunToModel never
				// populates the indexed scope levels (only ScopeExtra), and empty
				// NOT NULL columns that read as coverage and aren't are worse than
				// no columns at all.
				for _, table := range []string{
					"cortex_agents",
					"cortex_runs",
					"cortex_memories",
					"cortex_checkpoints",
				} {
					if _, err := exec.Exec(ctx, `
ALTER TABLE `+table+`
    ADD COLUMN IF NOT EXISTS scope_l0    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_l1    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_l2    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_extra JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS scope_canon TEXT  NOT NULL DEFAULT ''
`); err != nil {
						return fmt.Errorf("add scope columns to %s: %w", table, err)
					}
					if _, err := exec.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_`+table+`_scope ON `+table+` (scope_l0, scope_l1, scope_l2)
`); err != nil {
						return fmt.Errorf("index scope on %s: %w", table, err)
					}
				}

				// No backfill. tenant_id/app_id used a vocabulary this phase
				// deliberately doesn't carry forward — a host's scope keys
				// (e.g. "workspace=", "project=") are its own, and inventing
				// "tenant=" or "app=" values for old rows would make scope_l0
				// polysemous per row: some rows keyed by a host-defined level,
				// others by a fabricated one nothing ever queries for. Worse,
				// every pre-v2 conversation row had tenant_id = '', so a
				// literal 'tenant=' backfill would silently orphan all v1
				// history into an unreachable bucket. Pre-existing rows are
				// left with empty scope columns instead: every scoped query
				// requires a non-zero cortex.Scope on the context and applies
				// scopePredicates, so a row with scope_l0 = '' simply never
				// matches and is invisible until the host re-scopes it. See
				// docs/content/docs/concepts/multi-tenancy.mdx for the
				// cutover note hosts need to read before upgrading.
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				for _, table := range []string{
					"cortex_agents",
					"cortex_runs",
					"cortex_memories",
					"cortex_checkpoints",
				} {
					if _, err := exec.Exec(ctx, `
ALTER TABLE `+table+`
    DROP COLUMN IF EXISTS scope_l0,
    DROP COLUMN IF EXISTS scope_l1,
    DROP COLUMN IF EXISTS scope_l2,
    DROP COLUMN IF EXISTS scope_extra,
    DROP COLUMN IF EXISTS scope_canon
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
ALTER TABLE `+table+`
    ADD COLUMN IF NOT EXISTS scope_l0    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_l1    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_l2    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_extra JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS scope_canon TEXT  NOT NULL DEFAULT ''
`); err != nil {
						return fmt.Errorf("add scope columns to %s: %w", table, err)
					}
					if _, err := exec.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_`+table+`_scope ON `+table+` (scope_l0, scope_l1, scope_l2)
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
ALTER TABLE `+table+`
    DROP COLUMN IF EXISTS scope_l0,
    DROP COLUMN IF EXISTS scope_l1,
    DROP COLUMN IF EXISTS scope_l2,
    DROP COLUMN IF EXISTS scope_extra,
    DROP COLUMN IF EXISTS scope_canon
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
				//
				// CONCURRENTLY on both statements so this doesn't hold a
				// write lock on cortex_memories for the rebuild: grove's
				// pgmigrate executor runs each Exec call directly against
				// the pool/dedicated connection with no surrounding
				// BEGIN/COMMIT (see pgmigrate.Executor.Exec), so
				// CONCURRENTLY is available here -- but each statement must
				// be its own Exec call. Bundling DROP and CREATE into one
				// multi-statement string, like the rest of this file does,
				// would make pgx send them as a single simple-query
				// message, and Postgres implicitly wraps a multi-statement
				// simple-query message in a transaction block, which
				// CONCURRENTLY refuses to run inside.
				//
				// The DROP is unconditional, not gated on whether a prior
				// run got this far, and the CREATE has no IF NOT EXISTS.
				// A CONCURRENTLY build that dies partway (lock timeout,
				// killed connection, cancelled deploy) leaves an INVALID
				// index under this name -- Postgres does not clean it up.
				// With IF NOT EXISTS on the CREATE, a retry would see the
				// name already taken, emit a NOTICE, return no error, and
				// grove would record the migration as applied with the
				// unique index never actually built -- silently reopening
				// the exact cross-scope clobber this migration exists to
				// close. Dropping first guarantees a retry always starts
				// from nothing, and dropping IF NOT EXISTS from the CREATE
				// means a genuine build failure surfaces as a returned
				// error instead of a silent skip.
				//
				// This does leave cortex_memories with no unique index on
				// (agent_id, kind, key[, scope_canon]) for the WHERE
				// kind = 'working' rows between the DROP completing and the
				// CONCURRENTLY CREATE finishing. SaveWorking's ON CONFLICT
				// target names this index explicitly (see memory.go), so a
				// write landing in that window doesn't silently clobber
				// across scopes -- it fails outright with "there is no
				// unique or exclusion constraint matching the ON CONFLICT
				// specification" until the index exists again. That's a
				// brief write-availability gap on one table during a
				// migration, not a correctness regression, and it's judged
				// acceptable here.
				//
				// The alternative -- build a new index under a temporary
				// name, then swap names in a fast catalog-only transaction
				// -- would close that gap, but at a worse cost: it requires
				// leaving the OLD, unscoped index in force as the only
				// working unique constraint for the entire CONCURRENTLY
				// build, which is exactly the index that lets scope B
				// clobber scope A's working memory. A short fail-loud
				// availability gap is preferable to keeping the known
				// cross-scope bug exploitable for the full build duration,
				// so the extra complexity of a build-then-swap wasn't
				// taken.
				if _, err := exec.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS idx_cortex_memories_working`); err != nil {
					return fmt.Errorf("drop old working-memory index: %w", err)
				}
				if _, err := exec.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY idx_cortex_memories_working ON cortex_memories (agent_id, kind, key, scope_canon) WHERE kind = 'working'`); err != nil {
					return fmt.Errorf("create scope-aware working-memory index: %w", err)
				}
				return nil
			},
			Down: func(ctx context.Context, exec migrate.Executor) error {
				// Same unconditional-drop-then-unguarded-create shape as Up,
				// for the same reason: a failed CONCURRENTLY build must not
				// be able to leave an invalid index that IF NOT EXISTS then
				// silently treats as done.
				if _, err := exec.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS idx_cortex_memories_working`); err != nil {
					return fmt.Errorf("drop scope-aware working-memory index: %w", err)
				}
				if _, err := exec.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY idx_cortex_memories_working ON cortex_memories (agent_id, kind, key) WHERE kind = 'working'`); err != nil {
					return fmt.Errorf("recreate pre-scope working-memory index: %w", err)
				}
				return nil
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
				// (see 20260821000001). No backfill, same reasoning: a
				// fabricated level for pre-existing rows would make
				// scope_l0 polysemous per row and silently orphan old
				// rows instead of leaving them correctly invisible to
				// every scoped query until rescopeLegacyRows resolves
				// them.
				for _, table := range []string{
					"cortex_skills",
					"cortex_traits",
					"cortex_behaviors",
				} {
					if _, err := exec.Exec(ctx, `
ALTER TABLE `+table+`
    ADD COLUMN IF NOT EXISTS scope_l0    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_l1    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_l2    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_extra JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS scope_canon TEXT  NOT NULL DEFAULT ''
`); err != nil {
						return fmt.Errorf("add scope columns to %s: %w", table, err)
					}
					if _, err := exec.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_`+table+`_scope ON `+table+` (scope_l0, scope_l1, scope_l2)
`); err != nil {
						return fmt.Errorf("index scope on %s: %w", table, err)
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
ALTER TABLE `+table+`
    DROP COLUMN IF EXISTS scope_l0,
    DROP COLUMN IF EXISTS scope_l1,
    DROP COLUMN IF EXISTS scope_l2,
    DROP COLUMN IF EXISTS scope_extra,
    DROP COLUMN IF EXISTS scope_canon
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
				// (see 20260821000001). No backfill, same reasoning: a
				// fabricated level for pre-existing rows would make
				// scope_l0 polysemous per row and silently orphan old
				// rows instead of leaving them correctly invisible to
				// every scoped query until rescopeLegacyRows resolves
				// them.
				if _, err := exec.Exec(ctx, `
ALTER TABLE cortex_personas
    ADD COLUMN IF NOT EXISTS scope_l0    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_l1    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_l2    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS scope_extra JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS scope_canon TEXT  NOT NULL DEFAULT ''
`); err != nil {
					return fmt.Errorf("add scope columns to cortex_personas: %w", err)
				}
				if _, err := exec.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_cortex_personas_scope ON cortex_personas (scope_l0, scope_l1, scope_l2)
`); err != nil {
					return fmt.Errorf("index scope on cortex_personas: %w", err)
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
				if _, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_personas_scope_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cortex_personas_app_name ON cortex_personas (app_id, name);
`); err != nil {
					return fmt.Errorf("restore app_id-key unique index on cortex_personas: %w", err)
				}
				if _, err := exec.Exec(ctx, `
ALTER TABLE cortex_personas
    DROP COLUMN IF EXISTS scope_l0,
    DROP COLUMN IF EXISTS scope_l1,
    DROP COLUMN IF EXISTS scope_l2,
    DROP COLUMN IF EXISTS scope_extra,
    DROP COLUMN IF EXISTS scope_canon
`); err != nil {
					return fmt.Errorf("drop scope columns from cortex_personas: %w", err)
				}
				return nil
			},
		},
	)
	return g
}()
