package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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
