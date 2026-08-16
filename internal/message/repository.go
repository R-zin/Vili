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

func (r *Repository) GetByID(ctx context.Context, roomID uuid.UUID,limit int,before *time.Time) ([]*Message,error){
	if limit<=0|| limit > 100 {
		limit = 50 // Can be changed
	}
	var query string
	var args []any

	if before != nil && !before.IsZero() {
		query = ` SELECT m.id,.room_id,m.user_id,u.username,m.context,m.type,m.created_at
		        FROM messages m
				JOIN users u on m.user_id = u.id
				WHERE m.room_id = S1 and m.created_at < $2
				ORDER BY m.created_at DESC
				LIMIT $3;
		`
		args = []any{roomID,before,limit}
	}else {
		query = `SELECT m.id, m.room_id, m.user_id, u.username, m.content, m.type, m.created_at
			FROM messages m
			JOIN users u ON m.user_id = u.id
			WHERE m.room_id = $1
			ORDER BY m.created_at DESC
			LIMIT $2;`
			args = []any{roomID,limit}
	}
	rows,err := r.db.QueryContext(ctx,query,args...)
	if err != nil {
		return nil,fmt.Errorf("Failed query messages %w",err)
	}
	defer rows.Close()
	var messages []*Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID,&m.RoomID,&m.UserID,&m.Username,&m.Content,&m.Type,&m.CreatedAt); err != nil {
			return nil, fmt.Errorf("Failed to scan messages %w", err)
		}
		messages = append(messages,&m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	// Reverse the list so the client receives them in ascending chronological order (oldest to newest)
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages, nil

}