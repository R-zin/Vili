package room

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres SQLSTATEs used to map driver errors to sentinel errors.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

// roomColumns is the shared projection for scanning a Room.
const roomColumns = `id, name, description, is_private, (dm_key IS NOT NULL), created_by, created_at, updated_at`

// PostgresRepository is the production Repository backed by *sql.DB.
type PostgresRepository struct {
	db *sql.DB
}

// NewPostgresRepository builds a PostgresRepository.
func NewPostgresRepository(db *sql.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRoom(s rowScanner, r *Room) error {
	return s.Scan(&r.ID, &r.Name, &r.Description, &r.IsPrivate, &r.IsDM, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// inTx runs fn in a transaction, committing on success.
func (r *PostgresRepository) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Create inserts a room and its owner membership in one transaction. A
// duplicate name maps to ErrNameTaken.
func (r *PostgresRepository) Create(ctx context.Context, ownerID uuid.UUID, name, description string, private bool) (Room, error) {
	var room Room
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		if err := scanRoom(tx.QueryRowContext(ctx, `
			INSERT INTO rooms (id, name, description, is_private, created_by)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+roomColumns, uuid.New(), name, description, private, ownerID), &room); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO room_members (room_id, user_id, role) VALUES ($1, $2, 'owner')`, room.ID, ownerID)
		return err
	})
	if err != nil {
		if isPgCode(err, uniqueViolation) {
			return Room{}, ErrNameTaken
		}
		if isPgCode(err, foreignKeyViolation) {
			return Room{}, ErrUserNotFound // owner account no longer exists
		}
		return Room{}, fmt.Errorf("room: create: %w", err)
	}
	return room, nil
}

// escapeLike escapes LIKE metacharacters so a search term matches literally.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// List returns the rooms visible to userID with per-user counters.
func (r *PostgresRepository) List(ctx context.Context, userID uuid.UUID, f ListFilter) ([]Summary, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT r.id, r.name, r.description, r.is_private, (r.dm_key IS NOT NULL),
		       r.created_by, r.created_at, r.updated_at,
		       COALESCE(me.role, ''),
		       (SELECT count(*) FROM room_members c WHERE c.room_id = r.id),
		       CASE WHEN me.user_id IS NULL THEN 0 ELSE
		         (SELECT count(*) FROM messages m
		           WHERE m.room_id = r.id AND m.created_at > me.last_read_at
		             AND m.user_id <> $1 AND m.deleted_at IS NULL) END,
		       CASE WHEN me.user_id IS NULL THEN 0 ELSE
		         (SELECT count(*) FROM messages m
		           WHERE m.room_id = r.id AND m.created_at > me.last_read_at
		             AND m.user_id <> $1 AND m.deleted_at IS NULL
		             AND (SELECT username FROM users WHERE id = $1) = ANY (m.mentions)) END,
		       COALESCE((SELECT u.username FROM room_members pm JOIN users u ON u.id = pm.user_id
		                  WHERE pm.room_id = r.id AND pm.user_id <> $1 AND r.dm_key IS NOT NULL
		                  LIMIT 1), '')
		FROM rooms r
		LEFT JOIN room_members me ON me.room_id = r.id AND me.user_id = $1
		WHERE (me.user_id IS NOT NULL OR (NOT r.is_private AND NOT $2))
		  AND ($3 = '' OR r.name ILIKE '%' || $3 || '%')
		ORDER BY r.created_at DESC
		LIMIT $4`,
		userID, f.MineOnly, escapeLike(f.Query), limit)
	if err != nil {
		return nil, fmt.Errorf("room: list: %w", err)
	}
	defer rows.Close()

	out := make([]Summary, 0)
	for rows.Next() {
		var s Summary
		var role string
		if err := rows.Scan(&s.ID, &s.Name, &s.Description, &s.IsPrivate, &s.IsDM,
			&s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
			&role, &s.MemberCount, &s.Unread, &s.UnreadMentions, &s.Peer); err != nil {
			return nil, fmt.Errorf("room: list scan: %w", err)
		}
		s.Role = MemberRole(role)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("room: list rows: %w", err)
	}
	return out, nil
}

// ByID returns the room with the given id or ErrNotFound.
func (r *PostgresRepository) ByID(ctx context.Context, id uuid.UUID) (Room, error) {
	var room Room
	err := scanRoom(r.db.QueryRowContext(ctx, `SELECT `+roomColumns+` FROM rooms WHERE id = $1`, id), &room)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Room{}, ErrNotFound
		}
		return Room{}, fmt.Errorf("room: by id: %w", err)
	}
	return room, nil
}

// Role returns the member's role or ErrNotMember.
func (r *PostgresRepository) Role(ctx context.Context, roomID, userID uuid.UUID) (MemberRole, error) {
	var role string
	err := r.db.QueryRowContext(ctx,
		`SELECT role FROM room_members WHERE room_id = $1 AND user_id = $2`, roomID, userID).Scan(&role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotMember
		}
		return "", fmt.Errorf("room: role: %w", err)
	}
	return MemberRole(role), nil
}

// AddMember inserts the membership. A missing room (FK violation) maps to
// ErrNotFound; an existing membership maps to ErrAlreadyMember.
func (r *PostgresRepository) AddMember(ctx context.Context, roomID, userID uuid.UUID, role MemberRole) error {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO room_members (room_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (room_id, user_id) DO NOTHING`,
		roomID, userID, string(role),
	)
	if err != nil {
		if isPgCode(err, foreignKeyViolation) {
			return ErrNotFound
		}
		return fmt.Errorf("room: add member: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		if _, err := r.ByID(ctx, roomID); err != nil {
			return err
		}
		return ErrAlreadyMember
	}
	return nil
}

// RemoveMember deletes the membership and keeps the room healthy: ownership is
// handed on when the last owner leaves, and an emptied room is deleted.
func (r *PostgresRepository) RemoveMember(ctx context.Context, roomID, userID uuid.UUID) error {
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		var role string
		err := tx.QueryRowContext(ctx,
			`DELETE FROM room_members WHERE room_id = $1 AND user_id = $2 RETURNING role`,
			roomID, userID).Scan(&role)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				var exists bool
				if qerr := tx.QueryRowContext(ctx,
					`SELECT EXISTS(SELECT 1 FROM rooms WHERE id = $1)`, roomID).Scan(&exists); qerr != nil {
					return qerr
				}
				if !exists {
					return ErrNotFound
				}
				return ErrNotMember
			}
			return err
		}

		var remaining int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM room_members WHERE room_id = $1`, roomID).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			_, err := tx.ExecContext(ctx, `DELETE FROM rooms WHERE id = $1`, roomID)
			return err
		}
		if MemberRole(role) == RoleOwner {
			var owners int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM room_members WHERE room_id = $1 AND role = 'owner'`, roomID).Scan(&owners); err != nil {
				return err
			}
			if owners == 0 {
				_, err := tx.ExecContext(ctx, `
					UPDATE room_members SET role = 'owner'
					WHERE (room_id, user_id) = (
						SELECT room_id, user_id FROM room_members WHERE room_id = $1
						ORDER BY (role = 'admin') DESC, joined_at ASC, user_id ASC LIMIT 1)`, roomID)
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotMember) {
			return err
		}
		return fmt.Errorf("room: remove member: %w", err)
	}
	return nil
}

// Members lists a room's members, owners first, then admins, then by join time.
func (r *PostgresRepository) Members(ctx context.Context, roomID uuid.UUID) ([]Member, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT rm.user_id, u.username, rm.role, rm.joined_at
		FROM room_members rm JOIN users u ON u.id = rm.user_id
		WHERE rm.room_id = $1
		ORDER BY CASE rm.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, rm.joined_at, u.username`, roomID)
	if err != nil {
		return nil, fmt.Errorf("room: members: %w", err)
	}
	defer rows.Close()
	out := make([]Member, 0)
	for rows.Next() {
		var m Member
		var role string
		if err := rows.Scan(&m.UserID, &m.Username, &role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("room: members scan: %w", err)
		}
		m.Role = MemberRole(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetRole updates a member's role.
func (r *PostgresRepository) SetRole(ctx context.Context, roomID, userID uuid.UUID, role MemberRole) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE room_members SET role = $3 WHERE room_id = $1 AND user_id = $2`, roomID, userID, string(role))
	if err != nil {
		return fmt.Errorf("room: set role: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotMember
	}
	return nil
}

// Update applies the non-nil fields of p to a (non-DM) room.
func (r *PostgresRepository) Update(ctx context.Context, id uuid.UUID, p UpdateParams) (Room, error) {
	var room Room
	err := scanRoom(r.db.QueryRowContext(ctx, `
		UPDATE rooms SET
			name        = COALESCE($2, name),
			description = COALESCE($3, description),
			is_private  = COALESCE($4, is_private),
			updated_at  = now()
		WHERE id = $1 AND dm_key IS NULL
		RETURNING `+roomColumns, id, p.Name, p.Description, p.IsPrivate), &room)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Room{}, ErrNotFound
		}
		if isPgCode(err, uniqueViolation) {
			return Room{}, ErrNameTaken
		}
		return Room{}, fmt.Errorf("room: update: %w", err)
	}
	return room, nil
}

// Delete removes a room; memberships, messages and invites cascade.
func (r *PostgresRepository) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM rooms WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("room: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRead moves the member's read cursor to now.
func (r *PostgresRepository) MarkRead(ctx context.Context, roomID, userID uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE room_members SET last_read_at = now() WHERE room_id = $1 AND user_id = $2`, roomID, userID)
	if err != nil {
		return fmt.Errorf("room: mark read: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotMember
	}
	return nil
}

const inviteColumns = `id, room_id, created_by, expires_at, max_uses, uses, revoked_at, created_at`

func scanInvite(s rowScanner, i *Invite) error {
	return s.Scan(&i.ID, &i.RoomID, &i.CreatedBy, &i.ExpiresAt, &i.MaxUses, &i.Uses, &i.RevokedAt, &i.CreatedAt)
}

// CreateInvite stores an invite by code digest.
func (r *PostgresRepository) CreateInvite(ctx context.Context, roomID, createdBy uuid.UUID, codeHash string, expiresAt time.Time, maxUses int) (Invite, error) {
	var inv Invite
	err := scanInvite(r.db.QueryRowContext(ctx, `
		INSERT INTO room_invites (id, room_id, created_by, code_hash, expires_at, max_uses)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+inviteColumns, uuid.New(), roomID, createdBy, codeHash, expiresAt, maxUses), &inv)
	if err != nil {
		if isPgCode(err, foreignKeyViolation) {
			return Invite{}, ErrNotFound
		}
		return Invite{}, fmt.Errorf("room: create invite: %w", err)
	}
	return inv, nil
}

// ListInvites returns a room's invites, newest first.
func (r *PostgresRepository) ListInvites(ctx context.Context, roomID uuid.UUID) ([]Invite, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+inviteColumns+` FROM room_invites WHERE room_id = $1 ORDER BY created_at DESC LIMIT 100`, roomID)
	if err != nil {
		return nil, fmt.Errorf("room: list invites: %w", err)
	}
	defer rows.Close()
	out := make([]Invite, 0)
	for rows.Next() {
		var inv Invite
		if err := scanInvite(rows, &inv); err != nil {
			return nil, fmt.Errorf("room: list invites scan: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite marks an invite revoked.
func (r *PostgresRepository) RevokeInvite(ctx context.Context, roomID, inviteID uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE room_invites SET revoked_at = COALESCE(revoked_at, now())
		WHERE id = $1 AND room_id = $2`, inviteID, roomID)
	if err != nil {
		return fmt.Errorf("room: revoke invite: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RedeemInvite validates and consumes an invite and adds the member, all under
// a row lock so concurrent redemptions cannot exceed max_uses.
func (r *PostgresRepository) RedeemInvite(ctx context.Context, codeHash string, userID uuid.UUID) (Room, error) {
	var room Room
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		var (
			inviteID uuid.UUID
			roomID   uuid.UUID
			expires  time.Time
			maxUses  int
			uses     int
			revoked  *time.Time
		)
		err := tx.QueryRowContext(ctx, `
			SELECT id, room_id, expires_at, max_uses, uses, revoked_at
			FROM room_invites WHERE code_hash = $1 FOR UPDATE`, codeHash,
		).Scan(&inviteID, &roomID, &expires, &maxUses, &uses, &revoked)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInviteInvalid
			}
			return err
		}
		if revoked != nil || !time.Now().Before(expires) {
			return ErrInviteInvalid
		}
		// An existing member is told so (and consumes nothing) even when the
		// invite is already used up.
		var member bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM room_members WHERE room_id = $1 AND user_id = $2)`,
			roomID, userID).Scan(&member); err != nil {
			return err
		}
		if member {
			return ErrAlreadyMember
		}
		if maxUses > 0 && uses >= maxUses {
			return ErrInviteInvalid
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO room_members (room_id, user_id, role) VALUES ($1, $2, 'member')
			ON CONFLICT (room_id, user_id) DO NOTHING`, roomID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrAlreadyMember
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE room_invites SET uses = uses + 1 WHERE id = $1`, inviteID); err != nil {
			return err
		}
		return scanRoom(tx.QueryRowContext(ctx, `SELECT `+roomColumns+` FROM rooms WHERE id = $1`, roomID), &room)
	})
	if err != nil {
		if errors.Is(err, ErrInviteInvalid) || errors.Is(err, ErrAlreadyMember) {
			return Room{}, err
		}
		return Room{}, fmt.Errorf("room: redeem invite: %w", err)
	}
	return room, nil
}

// OpenDM finds or creates the DM room between userID and peerUsername.
func (r *PostgresRepository) OpenDM(ctx context.Context, userID uuid.UUID, peerUsername string) (Room, error) {
	var room Room
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		var peerID uuid.UUID
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM users WHERE username = $1`, peerUsername).Scan(&peerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUserNotFound
			}
			return err
		}
		if peerID == userID {
			return ErrSelfDM
		}
		a, b := userID.String(), peerID.String()
		if b < a {
			a, b = b, a
		}
		key := a + ":" + b

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rooms (id, name, description, is_private, dm_key, created_by)
			VALUES ($1, $2, '', true, $3, $4)
			ON CONFLICT (dm_key) DO NOTHING`,
			uuid.New(), "dm:"+key, key, userID); err != nil {
			return err
		}
		if err := scanRoom(tx.QueryRowContext(ctx,
			`SELECT `+roomColumns+` FROM rooms WHERE dm_key = $1`, key), &room); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO room_members (room_id, user_id, role)
			VALUES ($1, $2, 'member'), ($1, $3, 'member')
			ON CONFLICT (room_id, user_id) DO NOTHING`, room.ID, userID, peerID)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrSelfDM) {
			return Room{}, err
		}
		if isPgCode(err, foreignKeyViolation) {
			return Room{}, ErrUserNotFound
		}
		return Room{}, fmt.Errorf("room: open dm: %w", err)
	}
	return room, nil
}
