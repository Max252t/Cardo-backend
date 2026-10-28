CREATE INDEX board_events_board_created_idx ON board_events (board_id, created_at);
DROP INDEX IF EXISTS board_events_created_at_idx;
