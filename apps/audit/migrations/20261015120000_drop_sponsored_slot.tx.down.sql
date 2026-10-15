-- Recreates the sponsored slot tables exactly as migrations 20261012100000 and
-- 20261012120000 left them. Data is not restored.
CREATE TABLE sponsors (
    id            BIGSERIAL PRIMARY KEY,
    provider      TEXT NOT NULL CHECK (char_length(provider) BETWEEN 1 AND 80),
    pitch         TEXT NOT NULL CHECK (char_length(pitch) BETWEEN 1 AND 200),
    -- Where the visitor ends up. May be an affiliate or tracking link. Visitors
    -- and API callers are sent through /go/sponsor first, which counts the
    -- click; this is the destination of that redirect.
    url           TEXT NOT NULL CHECK (url ~ '^https://'),
    -- Optional. A model id and base URL the sponsor wants shown, for a pitch
    -- that is a concrete endpoint ("try Foo-8B at https://api.example.com/v1").
    model         TEXT NOT NULL DEFAULT '',
    base_url      TEXT NOT NULL DEFAULT '' CHECK (base_url = '' OR base_url ~ '^https://'),
    -- Optional slug of an endpoint already on the shelf. When it matches, the
    -- slot shows that endpoint's real probe data next to the label. When it
    -- does not, the slot shows none: a missing probe is never filled in.
    endpoint_slug TEXT NOT NULL DEFAULT '',
    -- Inclusive on both ends, in UTC dates.
    starts_on     DATE NOT NULL,
    ends_on       DATE NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (ends_on >= starts_on),
    -- One owner at a time, enforced by the database: two sponsors whose dates
    -- overlap cannot both be stored, so "which one is showing" never depends on
    -- a tiebreak somebody forgot to write.
    CONSTRAINT sponsors_one_owner_at_a_time
        EXCLUDE USING gist (daterange(starts_on, ends_on, '[]') WITH &&)
);

CREATE INDEX idx_sponsors_window ON sponsors(starts_on, ends_on);

ALTER TABLE sponsors ADD COLUMN subscription_id TEXT UNIQUE;

-- One row per subscription we have seen, whatever became of it. It is the
-- record that survives a rejected order: fields that failed validation are never
-- published, but they are kept here, with the reason, so the order can be fixed
-- by hand instead of vanishing.
CREATE TABLE sponsor_orders (
    subscription_id TEXT PRIMARY KEY,
    -- live: a slot exists. rejected: the fields failed validation, nothing was
    -- published. blocked: the fields were fine but another sponsor holds the
    -- dates. ended: the slot was ended by cancellation or revocation.
    status          TEXT NOT NULL CHECK (status IN ('live', 'rejected', 'blocked', 'ended')),
    reason          TEXT NOT NULL DEFAULT '',
    provider        TEXT NOT NULL DEFAULT '',
    pitch           TEXT NOT NULL DEFAULT '',
    url             TEXT NOT NULL DEFAULT '',
    polar_status    TEXT NOT NULL DEFAULT '',
    period_end      TIMESTAMPTZ,
    sponsor_id      BIGINT REFERENCES sponsors(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Webhook delivery ids already processed. Polar retries a delivery it did not
-- see succeed under the same id, so a second arrival of an id is answered 200
-- and not applied again.
CREATE TABLE webhook_deliveries (
    id          TEXT PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
