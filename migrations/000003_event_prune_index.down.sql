CREATE INDEX board_events_created_at_idx ON board_events (created_at);
DROP INDEX IF EXISTS board_events_prune_idx;
