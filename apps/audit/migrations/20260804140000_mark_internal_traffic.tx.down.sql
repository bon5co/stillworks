DROP INDEX idx_traffic_events_internal;

ALTER TABLE traffic_events DROP COLUMN is_internal;
