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
