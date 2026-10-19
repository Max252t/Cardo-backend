CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
  id            UUID PRIMARY KEY,
  email         CITEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  display_name  TEXT NOT NULL CHECK (length(display_name) BETWEEN 1 AND 100),
  avatar_color  TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE refresh_tokens (
  id          UUID PRIMARY KEY,
  user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  family_id   UUID NOT NULL,
  token_hash  TEXT NOT NULL UNIQUE,
  expires_at  TIMESTAMPTZ NOT NULL,
  revoked_at  TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_family_id_idx ON refresh_tokens (family_id);

CREATE TABLE boards (
  id          UUID PRIMARY KEY,
  title       TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
  owner_id    UUID NOT NULL REFERENCES users(id),
  seq         BIGINT NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at  TIMESTAMPTZ
);

CREATE TABLE board_members (
  board_id  UUID NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role      TEXT NOT NULL CHECK (role IN ('owner','editor','viewer')),
  joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (board_id, user_id)
);
CREATE INDEX board_members_user_id_idx ON board_members (user_id);

CREATE TABLE invites (
  id          UUID PRIMARY KEY,
  board_id    UUID NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  token_hash  TEXT NOT NULL UNIQUE,
  role        TEXT NOT NULL CHECK (role IN ('editor','viewer')),
  created_by  UUID NOT NULL REFERENCES users(id),
  expires_at  TIMESTAMPTZ NOT NULL,
  max_uses    INT NOT NULL DEFAULT 1,
  uses        INT NOT NULL DEFAULT 0,
  revoked_at  TIMESTAMPTZ
);

CREATE TABLE columns (
  id          UUID PRIMARY KEY,
  board_id    UUID NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  title       TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
  rank        TEXT COLLATE "C" NOT NULL,
  version     BIGINT NOT NULL DEFAULT 1,
  archived_at TIMESTAMPTZ,
  UNIQUE (id, board_id)
);
CREATE UNIQUE INDEX columns_board_id_rank_key ON columns (board_id, rank) WHERE archived_at IS NULL;

CREATE TABLE cards (
  id                  UUID PRIMARY KEY,
  board_id            UUID NOT NULL,
  column_id           UUID NOT NULL,
  title               TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 500),
  description         TEXT NOT NULL DEFAULT '' CHECK (length(description) <= 10000),
  rank                TEXT COLLATE "C" NOT NULL,
  version             BIGINT NOT NULL DEFAULT 1,
  description_version BIGINT NOT NULL DEFAULT 1,
  created_by          UUID NOT NULL REFERENCES users(id),
  updated_by          UUID NOT NULL REFERENCES users(id),
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  archived_at         TIMESTAMPTZ,
  FOREIGN KEY (column_id, board_id) REFERENCES columns (id, board_id)
);
CREATE UNIQUE INDEX cards_column_id_rank_key ON cards (column_id, rank) WHERE archived_at IS NULL;
CREATE INDEX cards_board_id_idx ON cards (board_id) WHERE archived_at IS NULL;

CREATE TABLE board_events (
  board_id   UUID NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  seq        BIGINT NOT NULL,
  type       TEXT NOT NULL,
  payload    JSONB NOT NULL,
  op_id      UUID,
  actor_id   UUID REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (board_id, seq)
);
CREATE INDEX board_events_created_at_idx ON board_events (created_at);

CREATE TABLE processed_ops (
  op_id      UUID PRIMARY KEY,
  board_id   UUID NOT NULL,
  user_id    UUID NOT NULL,
  outcome    JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX processed_ops_created_at_idx ON processed_ops (created_at);
