-- The sponsored slot: one paid placement on the shelf and in /api/llm/up.
--
-- It lives in the database and not in configuration so that whoever sells the
-- slot -- a person, or an agent working from the sponsor's email -- can change
-- it with one command and no deploy. A deploy to swap one sentence is a deploy
-- that does not get done on the day the sponsor pays.
--
-- A sponsor is never a probed row. This table holds what somebody paid to say;
-- llm_endpoints and llm_models hold what we measured. They are separate tables
-- so that no query can join one into the other by accident, and the sponsor's
-- optional endpoint_slug is a lookup key for showing the REAL probe data, never
-- a foreign key that could promote a sponsor into the ranked list.
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
