CREATE TABLE llm_endpoints (
    id                SERIAL PRIMARY KEY,
    slug              TEXT NOT NULL UNIQUE,
    provider          TEXT NOT NULL,
    base_url          TEXT NOT NULL,
    chat_path         TEXT NOT NULL DEFAULT '/v1/chat/completions',
    models_path       TEXT NOT NULL DEFAULT '/v1/models',
    -- v1 shelf is keyless only: 'none' means usable with no signup and no key.
    auth_mode         TEXT NOT NULL DEFAULT 'none',
    default_model     TEXT NOT NULL DEFAULT '',
    docs_url          TEXT NOT NULL DEFAULT '',
    notes             TEXT NOT NULL DEFAULT '',
    openai_compatible BOOLEAN NOT NULL DEFAULT TRUE,
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Keyless is a property of a MODEL, not of a provider. Verified on llm7.io
-- 2026-08-02: 4 of 35 models answer without a key, the other 31 return 401
-- missing_api_key. Every list on the internet records this as one green tick
-- against the provider. Model ids also move -- gpt-4o-mini and gpt-5-nano both
-- vanished from that same listing -- so last_seen is what makes a disappearance
-- visible instead of silent.
CREATE TABLE llm_models (
    id           SERIAL PRIMARY KEY,
    endpoint_id  INTEGER NOT NULL REFERENCES llm_endpoints(id) ON DELETE CASCADE,
    model_id     TEXT NOT NULL,
    tier         TEXT NOT NULL DEFAULT '',
    -- NULL means never verified, which is not the same as false.
    keyless      BOOLEAN,
    first_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_checked TIMESTAMPTZ,
    last_ok      TIMESTAMPTZ,
    UNIQUE (endpoint_id, model_id)
);

CREATE INDEX idx_llm_models_endpoint ON llm_models(endpoint_id);

-- Append-only. An endpoint that dies between runs must show up as history,
-- not as a silently overwritten row: the track record is the whole product.
CREATE TABLE llm_probes (
    id               BIGSERIAL PRIMARY KEY,
    endpoint_id      INTEGER NOT NULL REFERENCES llm_endpoints(id) ON DELETE CASCADE,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    kind             TEXT NOT NULL,
    outcome          TEXT NOT NULL,
    http_status      INTEGER NOT NULL DEFAULT 0,
    latency_ms       INTEGER NOT NULL DEFAULT 0,
    model_used       TEXT NOT NULL DEFAULT '',
    models_listed    INTEGER NOT NULL DEFAULT 0,
    completion_chars INTEGER NOT NULL DEFAULT 0,
    error            TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_llm_probes_endpoint ON llm_probes(endpoint_id, started_at DESC);
CREATE INDEX idx_llm_probes_started ON llm_probes(started_at DESC);
