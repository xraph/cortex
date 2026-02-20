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
