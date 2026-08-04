-- Traffic is measured here, in the same binary and the same database as the
-- probes, for the same reason the prober lives inside the server: an outcome
-- that depends on a third-party script the visitor's browser may never run, or
-- on a service somebody has to remember to keep alive, is an outcome that
-- quietly stops being measured. The audience for this site is agents calling
-- /api/llm/up, and an agent never executes JavaScript, so a client-side
-- analytics tag could not see the metric that matters at all.
CREATE TABLE traffic_events (
    id            BIGSERIAL PRIMARY KEY,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 'api' or 'page'. The two are counted separately because they are two
    -- different products: the runtime API is the thesis, the shelf is the
    -- shop window that sends people to it.
    kind          TEXT NOT NULL,
    path          TEXT NOT NULL,
    status        INTEGER NOT NULL DEFAULT 0,
    -- A salted, daily-rotating hash of IP + user agent. The raw address is
    -- never written down: this site tells other people's users what it checked
    -- and when, and it would be a poor look to log more about the visitor than
    -- the providers being audited log about us.
    visitor_hash  TEXT NOT NULL,
    referrer_host TEXT NOT NULL DEFAULT '',
    user_agent    TEXT NOT NULL DEFAULT '',
    -- Crawlers are recorded, never counted. Deleting them would hide a traffic
    -- source; counting them would turn Googlebot into an audience.
    is_crawler    BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX idx_traffic_events_occurred ON traffic_events(occurred_at DESC);
CREATE INDEX idx_traffic_events_kind ON traffic_events(kind, occurred_at DESC);
CREATE INDEX idx_traffic_events_visitor ON traffic_events(visitor_hash);

-- One row, created on first start and never rotated. Without a secret in the
-- hash input, IP + user agent + date is brute-forceable over the whole IPv4
-- space in seconds, and the "we do not store addresses" claim would be a lie
-- told in good faith. The date is part of the hash input instead, so the salt
-- can stay put while the identifier still changes every day.
CREATE TABLE traffic_salt (
    id         SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    salt       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
