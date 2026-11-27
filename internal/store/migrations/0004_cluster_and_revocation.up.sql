-- Multi-instance realtime and immediate access-token revocation.

-- Bumped whenever all of a user's sessions must end at once (password change,
-- "sign out everywhere"). Access tokens carry the version they were minted
-- with and are rejected once it no longer matches.
ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version integer NOT NULL DEFAULT 0;

-- Live server instances, kept fresh by a heartbeat. Presence rows of an
-- instance that stopped heartbeating (crashed) are ignored and later purged.
CREATE TABLE IF NOT EXISTS ws_instances (
    id           text PRIMARY KEY,
    started_at   timestamptz NOT NULL DEFAULT now(),
    heartbeat_at timestamptz NOT NULL DEFAULT now()
);

-- Who is connected where: one row per (instance, room, user) with the number
-- of that user's sockets on that instance.
CREATE TABLE IF NOT EXISTS ws_presence (
    instance_id text NOT NULL REFERENCES ws_instances (id) ON DELETE CASCADE,
    room_id     uuid NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    username    text NOT NULL,
    conns       integer NOT NULL CHECK (conns >= 0),
    PRIMARY KEY (instance_id, room_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_ws_presence_room ON ws_presence (room_id);

-- Bus payloads too large for a NOTIFY (8000-byte limit); the notification
-- carries only the row id. Short-lived: purged by the janitor.
CREATE TABLE IF NOT EXISTS bus_outbox (
    id         bigserial PRIMARY KEY,
    payload    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_bus_outbox_created ON bus_outbox (created_at);
