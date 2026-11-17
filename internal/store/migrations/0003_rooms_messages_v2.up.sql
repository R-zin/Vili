-- Phase 6: private rooms, invites, direct messages, read cursors, message
-- edit/delete/reply/mentions and full-text search.

-- Rooms: private rooms are invisible to non-members; dm_key marks a direct
-- message room and is unique so two users share at most one DM room.
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS is_private boolean NOT NULL DEFAULT false;
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS dm_key text UNIQUE;

-- Read cursor: messages newer than last_read_at (by someone else) are unread.
-- Existing members start fully caught up.
ALTER TABLE room_members ADD COLUMN IF NOT EXISTS last_read_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS idx_room_members_user ON room_members (user_id);

-- Invite codes. Only the SHA-256 digest of a code is stored, so a database
-- leak does not yield usable invites. max_uses = 0 means unlimited.
CREATE TABLE IF NOT EXISTS room_invites (
    id         uuid PRIMARY KEY,
    room_id    uuid NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    created_by uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  text NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    max_uses   integer NOT NULL DEFAULT 0 CHECK (max_uses >= 0),
    uses       integer NOT NULL DEFAULT 0 CHECK (uses >= 0),
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_room_invites_room ON room_invites (room_id);

-- Messages: edit/soft-delete markers, replies and resolved @mentions.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS edited_at  timestamptz;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS reply_to   uuid REFERENCES messages (id) ON DELETE SET NULL;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS mentions   text[] NOT NULL DEFAULT '{}';

-- Full-text search over message content ('simple' config: no stemming, so code
-- identifiers and non-English text index predictably).
ALTER TABLE messages ADD COLUMN IF NOT EXISTS tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED;
CREATE INDEX IF NOT EXISTS idx_messages_tsv ON messages USING gin (tsv);
CREATE INDEX IF NOT EXISTS idx_messages_reply_to ON messages (reply_to) WHERE reply_to IS NOT NULL;
