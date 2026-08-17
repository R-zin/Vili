-- Core schema for the Vili Phase-1 backend.
--
-- Username is intentionally NOT denormalized onto messages; readers JOIN
-- users at read time. Foreign keys use ON DELETE CASCADE so removing a user
-- or room cleans up dependent rows.

CREATE TABLE IF NOT EXISTS users (
    id            uuid PRIMARY KEY,
    username      text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rooms (
    id          uuid PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_by  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS room_members (
    room_id   uuid NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role      text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member')),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, user_id)
);

CREATE TABLE IF NOT EXISTS messages (
    id         uuid PRIMARY KEY,
    room_id    uuid NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    content    text NOT NULL,
    type       text NOT NULL DEFAULT 'text' CHECK (type IN ('text', 'diff', 'code', 'log', 'commit')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Supports the room history query: latest messages in a room, newest first.
CREATE INDEX IF NOT EXISTS idx_messages_room_created_at
    ON messages (room_id, created_at DESC);
