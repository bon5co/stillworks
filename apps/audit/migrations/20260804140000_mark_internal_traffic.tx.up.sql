-- The numbers on /stats are what decides whether this project keeps being
-- built. On day one they read 15 visitors, and every one of them was us --
-- deploy checks, browser tests, curl against the live box. Counting our own
-- work as demand is the most comfortable way to fail: the target gets hit,
-- nobody arrives, and the kill rule never fires.
--
-- Internal requests are recorded and excluded, exactly like crawlers. Deleting
-- them would hide that the checks happen at all; counting them would be a lie
-- told to ourselves.
ALTER TABLE traffic_events ADD COLUMN is_internal BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX idx_traffic_events_internal ON traffic_events(is_internal);
