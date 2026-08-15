package message

import (
	"time"
	"github.com/google/uuid"
	"context"
	"database/sql"
	"fmt"
	"errors"
)

var (
	ErrNotFound = errors.New("message not found")
)

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Create(ctx context.Context, message *Message) error {
	query := `INSERT INTO messages (id, room_id, user_id, username, content, type, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at;`
	if message.ID == uuid.Nil {
		message.ID = uuid.New()
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	if message.Type == "" {
		message.Type = TypeText
	}
	err := r.db.QueryRowContext(ctx, query, message.ID, message.RoomID, message.UserID, message.Username, message.Content, message.Type, message.CreatedAt).Scan(&message.ID, &message.CreatedAt)
	if err != nil {
		return fmt.Errorf("failed to create message: %w", err)
	}
	return nil
}
