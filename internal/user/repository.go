package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/R-zin/vili/internal/auth"
)

// uniqueViolation is the Postgres SQLSTATE for unique-constraint violations.
const uniqueViolation = "23505"

// PostgresRepository is the production Repository backed by *sql.DB.
type PostgresRepository struct {
	db *sql.DB
}

// NewPostgresRepository builds a PostgresRepository.
func NewPostgresRepository(db *sql.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

// Create inserts a user, generating the id, and returns the stored row. A
// duplicate username maps to ErrUsernameTaken.
func (r *PostgresRepository) Create(ctx context.Context, username, passwordHash string) (User, error) {
	u := User{ID: uuid.New(), Username: username, PasswordHash: passwordHash}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO users (id, username, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id, username, password_hash, created_at`,
		u.ID, u.Username, u.PasswordHash,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return User{}, ErrUsernameTaken
		}
		return User{}, fmt.Errorf("user: create: %w", err)
	}
	return u, nil
}

// ByID returns the user with the given id or ErrNotFound.
func (r *PostgresRepository) ByID(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	err := r.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, created_at
		FROM users
		WHERE id = $1`, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("user: by id: %w", err)
	}
	return u, nil
}

// UsernameByID returns just the username for the given id or ErrNotFound.
func (r *PostgresRepository) UsernameByID(ctx context.Context, id uuid.UUID) (string, error) {
	var username string
	err := r.db.QueryRowContext(ctx,
		`SELECT username FROM users WHERE id = $1`, id,
	).Scan(&username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("user: username by id: %w", err)
	}
	return username, nil
}

// ByUsername returns the user with the given username or ErrNotFound.
func (r *PostgresRepository) ByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := r.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, created_at
		FROM users
		WHERE username = $1`, username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("user: by username: %w", err)
	}
	return u, nil
}

// InsertRefreshToken stores a new refresh-token digest in the given family and
// returns its id. Only the digest is persisted, never the raw token.
func (r *PostgresRepository) InsertRefreshToken(ctx context.Context, userID, familyID uuid.UUID, hash string, expiresAt time.Time) (uuid.UUID, error) {
	id := uuid.New()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		id, userID, familyID, hash, expiresAt,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("user: insert refresh token: %w", err)
	}
	return id, nil
}

// RefreshTokenByHash returns the token row for a digest, or
// auth.ErrRefreshNotFound when absent.
func (r *PostgresRepository) RefreshTokenByHash(ctx context.Context, hash string) (auth.RefreshToken, error) {
	var t auth.RefreshToken
	var consumedAt *time.Time
	err := r.db.QueryRowContext(ctx, `
		SELECT id, user_id, family_id, expires_at, consumed_at
		FROM refresh_tokens
		WHERE token_hash = $1`, hash,
	).Scan(&t.ID, &t.UserID, &t.FamilyID, &t.ExpiresAt, &consumedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.RefreshToken{}, auth.ErrRefreshNotFound
		}
		return auth.RefreshToken{}, fmt.Errorf("user: refresh token by hash: %w", err)
	}
	t.Consumed = consumedAt != nil
	return t, nil
}

// ConsumeRefreshToken marks a token used and links it to its replacement.
func (r *PostgresRepository) ConsumeRefreshToken(ctx context.Context, id, replacedBy uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE refresh_tokens
		SET consumed_at = now(), replaced_by = $2
		WHERE id = $1`,
		id, replacedBy,
	)
	if err != nil {
		return fmt.Errorf("user: consume refresh token: %w", err)
	}
	return nil
}

// RevokeRefreshFamily deletes every token in a family (reuse detected).
func (r *PostgresRepository) RevokeRefreshFamily(ctx context.Context, familyID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM refresh_tokens WHERE family_id = $1`, familyID,
	)
	if err != nil {
		return fmt.Errorf("user: revoke refresh family: %w", err)
	}
	return nil
}

// RevokeRefreshToken deletes a single token by digest (logout). A missing token
// is not an error so logout stays idempotent.
func (r *PostgresRepository) RevokeRefreshToken(ctx context.Context, hash string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM refresh_tokens WHERE token_hash = $1`, hash,
	)
	if err != nil {
		return fmt.Errorf("user: revoke refresh token: %w", err)
	}
	return nil
}
