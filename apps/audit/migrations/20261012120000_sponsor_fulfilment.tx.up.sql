-- Automatic fulfilment of the sponsored slot from Polar subscription webhooks.
--
-- A paying sponsor with an empty slot is a broken product, so the slot is filled
-- by the purchase itself rather than by someone remembering to run a command.
--
-- sponsors.subscription_id ties a slot to the subscription that paid for it, so
-- every later event (renewal, cancellation, revocation) finds the same row and
-- delivery is idempotent. NULL for a sponsor set by hand with `manage sponsor`.
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
