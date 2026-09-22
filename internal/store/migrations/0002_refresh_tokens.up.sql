-- Phase 6: refresh tokens (Feature 1).
--
-- Refresh tokens are cryptographically random 256-bit values minted at login
-- and rotated on every use. Only the hex SHA-256 digest is stored, so a
-- read-only database leak never yields a usable token. family_id groups the
-- tokens of one login session: presenting an already-consumed token means the
-- token was stolen and replayed, so the whole family is revoked (forcing
-- re-login on every device in that chain). token_hash is unique so a presented
-- token maps to at most one row.

CREATE TABLE IF NOT EXISTS refresh_tokens (
    id          uuid PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id   uuid NOT NULL,
    token_hash  text NOT NULL UNIQUE,           -- hex(sha256(token))
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,                    -- set on rotation; NULL = still usable
    replaced_by uuid,                           -- child token id after rotation
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user   ON refresh_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family ON refresh_tokens (family_id);
