package room

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres SQLSTATEs used to map driver errors to sentinel errors.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

// PostgresRepository is the production Repository backed by *sql.DB.
type PostgresRepository struct {
	db *sql.DB
}

// NewPostgresRepository builds a PostgresRepository.
func NewPostgresRepository(db *sql.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

// Create inserts a room with a generated id and returns the stored row. A
// duplicate name maps to ErrNameTaken.
func (r *PostgresRepository) Create(ctx context.Context, ownerID uuid.UUID, name, description string) (Room, error) {
	room := Room{ID: uuid.New(), Name: name, Description: description, CreatedBy: ownerID}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO rooms (id, name, description, created_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id, name, description, created_by, created_at, updated_at`,
		room.ID, room.Name, room.Description, room.CreatedBy,
	).Scan(&room.ID, &room.Name, &room.Description, &room.CreatedBy, &room.CreatedAt, &room.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return Room{}, ErrNameTaken
		}
		return Room{}, fmt.Errorf("room: create: %w", err)
	}
	return room, nil
}

// List returns all rooms ordered by creation time, newest first.
func (r *PostgresRepository) List(ctx context.Context) ([]Room, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, description, created_by, created_at, updated_at
		FROM rooms
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("room: list: %w", err)
	}
	defer rows.Close()

	rooms := make([]Room, 0)
	for rows.Next() {
		var room Room
		if err := rows.Scan(&room.ID, &room.Name, &room.Description, &room.CreatedBy, &room.CreatedAt, &room.UpdatedAt); err != nil {
			return nil, fmt.Errorf("room: list scan: %w", err)
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("room: list rows: %w", err)
	}
	return rooms, nil
}

// ByID returns the room with the given id or ErrNotFound.
func (r *PostgresRepository) ByID(ctx context.Context, id uuid.UUID) (Room, error) {
	var room Room
	err := r.db.QueryRowContext(ctx, `
		SELECT id, name, description, created_by, created_at, updated_at
		FROM rooms
		WHERE id = $1`, id,
	).Scan(&room.ID, &room.Name, &room.Description, &room.CreatedBy, &room.CreatedAt, &room.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Room{}, ErrNotFound
		}
		return Room{}, fmt.Errorf("room: by id: %w", err)
	}
	return room, nil
}

// AddMember upserts nothing: it inserts the membership. A missing room (FK
// violation) maps to ErrNotFound; an existing membership maps to
// ErrAlreadyMember.
func (r *PostgresRepository) AddMember(ctx context.Context, roomID, userID uuid.UUID, role MemberRole) error {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO room_members (room_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (room_id, user_id) DO NOTHING`,
		roomID, userID, string(role),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return ErrNotFound
		}
		return fmt.Errorf("room: add member: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// The row already existed (conflict did nothing). Confirm the room
		// exists so we can report the right error.
		if _, err := r.ByID(ctx, roomID); err != nil {
			return err
		}
		return ErrAlreadyMember
	}
	return nil
}

// RemoveMember deletes the membership. Unknown rooms map to ErrNotFound;
// absent memberships map to ErrNotMember.
func (r *PostgresRepository) RemoveMember(ctx context.Context, roomID, userID uuid.UUID) error {
	if _, err := r.ByID(ctx, roomID); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM room_members
		WHERE room_id = $1 AND user_id = $2`,
		roomID, userID,
	)
	if err != nil {
		return fmt.Errorf("room: remove member: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotMember
	}
	return nil
}
