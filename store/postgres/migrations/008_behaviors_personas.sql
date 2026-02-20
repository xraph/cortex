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
