CREATE UNIQUE INDEX board_members_single_owner_idx ON board_members (board_id) WHERE role = 'owner';
