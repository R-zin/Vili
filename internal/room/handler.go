package room

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/respond"
)

// Validation bounds for room fields and listings.
const (
	nameMinLen = 1
	nameMaxLen = 120
	descMaxLen = 500

	defaultListLimit = 100
	maxListLimit     = 200

	// Invite bounds: default 7 days, at most 30; max_uses capped to keep codes
	// from being effectively permanent open doors.
	defaultInviteTTL  = 7 * 24 * time.Hour
	maxInviteTTL      = 30 * 24 * time.Hour
	maxInviteMaxUses  = 1000
	inviteCodeEntropy = 12 // bytes (96 bits)

	// reservedNamePrefix is owned by direct-message rooms.
	reservedNamePrefix = "dm:"
)

// Disconnector drops live realtime connections when membership changes, so a
// kicked or departed user stops receiving a room's traffic immediately. It is
// satisfied by the ws hub; nil disables it (REST-only deployments, tests).
type Disconnector interface {
	// Disconnect closes userID's connections to roomID.
	Disconnect(roomID, userID uuid.UUID)
	// CloseRoom closes every connection to roomID (the room was deleted).
	CloseRoom(roomID uuid.UUID)
}

// Handler serves the room feature's routes. All of them are protected; the
// wiring layer mounts them behind the auth middleware, so a user id is always
// present in the request context.
type Handler struct {
	repo Repository
	disc Disconnector
	now  func() time.Time
}

// NewHandler builds a room Handler. disc may be nil.
func NewHandler(repo Repository, disc Disconnector) *Handler {
	return &Handler{repo: repo, disc: disc, now: time.Now}
}

// RegisterRoutes mounts the protected room routes on the mux, each wrapped in
// the auth middleware provided by require.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, require func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /v1/createroom", require(h.create))
	mux.Handle("GET /v1/listrooms", require(h.list))
	mux.Handle("POST /v1/rooms/{id}/join", require(h.join))
	mux.Handle("POST /v1/rooms/{id}/leave", require(h.leave))
	mux.Handle("PATCH /v1/rooms/{id}", require(h.update))
	mux.Handle("DELETE /v1/rooms/{id}", require(h.delete))
	mux.Handle("POST /v1/rooms/{id}/read", require(h.markRead))
	mux.Handle("GET /v1/rooms/{id}/members", require(h.members))
	mux.Handle("DELETE /v1/rooms/{id}/members/{userID}", require(h.kick))
	mux.Handle("PUT /v1/rooms/{id}/members/{userID}/role", require(h.setRole))
	mux.Handle("POST /v1/rooms/{id}/invites", require(h.createInvite))
	mux.Handle("GET /v1/rooms/{id}/invites", require(h.listInvites))
	mux.Handle("DELETE /v1/rooms/{id}/invites/{inviteID}", require(h.revokeInvite))
	mux.Handle("POST /v1/invites/redeem", require(h.redeemInvite))
	mux.Handle("POST /v1/dms", require(h.openDM))
	// Realtime: GET /v1/rooms/{id}/ws is served by the ws feature package and
	// mounted by the api wiring layer, not here.
}

func (h *Handler) caller(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
	}
	return userID, ok
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return false
	}
	return true
}

// authorize resolves the room id and caller, then requires at least minRole.
// A non-member (or unknown room) gets the same 404 so membership and private
// rooms are not enumerable; a member with too low a role gets 403.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, minRole MemberRole) (roomID, userID uuid.UUID, role MemberRole, ok bool) {
	roomID, ok = pathUUID(w, r, "id", "room id")
	if !ok {
		return
	}
	userID, ok = h.caller(w, r)
	if !ok {
		return
	}
	role, err := h.repo.Role(r.Context(), roomID, userID)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			respond.Error(w, http.StatusNotFound, "not_found", "room not found or you are not a member")
		} else {
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not check membership", err)
		}
		return uuid.Nil, uuid.Nil, "", false
	}
	if role.rank() < minRole.rank() {
		respond.Error(w, http.StatusForbidden, "forbidden", "you do not have permission to do that in this room")
		return uuid.Nil, uuid.Nil, "", false
	}
	return roomID, userID, role, true
}

type createRoomRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsPrivate   bool   `json:"is_private"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req createRoomRequest
	if !decode(w, r, &req) {
		return
	}
	if err := validateRoom(req.Name, req.Description); err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}

	room, err := h.repo.Create(r.Context(), userID, req.Name, req.Description, req.IsPrivate)
	if err != nil {
		if errors.Is(err, ErrNameTaken) {
			respond.Error(w, http.StatusConflict, "conflict", "a room with that name already exists")
			return
		}
		if errors.Is(err, ErrUserNotFound) {
			respond.Error(w, http.StatusUnauthorized, "unauthorized", "account no longer exists")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not create room", err)
		return
	}
	respond.JSON(w, http.StatusCreated, room)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := defaultListLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			respond.Error(w, http.StatusBadRequest, "validation", "limit must be a positive integer")
			return
		}
		limit = min(n, maxListLimit)
	}
	query := strings.TrimSpace(q.Get("q"))
	if len(query) > nameMaxLen {
		respond.Error(w, http.StatusBadRequest, "validation", "q is too long")
		return
	}
	mine := q.Get("mine") == "1" || q.Get("mine") == "true"

	rooms, err := h.repo.List(r.Context(), userID, ListFilter{Query: query, MineOnly: mine, Limit: limit})
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not list rooms", err)
		return
	}
	respond.JSON(w, http.StatusOK, rooms)
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	roomID, ok := pathUUID(w, r, "id", "room id")
	if !ok {
		return
	}
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}

	// Private rooms (including DMs) cannot be joined directly: they answer 404
	// exactly like an unknown room so their existence is not revealed.
	room, err := h.repo.ByID(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "room not found")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not join room", err)
		return
	}
	if room.IsPrivate {
		if role, err := h.repo.Role(r.Context(), roomID, userID); err == nil && role != "" {
			respond.Error(w, http.StatusConflict, "conflict", "you are already a member of this room")
			return
		}
		respond.Error(w, http.StatusNotFound, "not_found", "room not found")
		return
	}

	if err := h.repo.AddMember(r.Context(), roomID, userID, RoleMember); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			respond.Error(w, http.StatusNotFound, "not_found", "room not found")
		case errors.Is(err, ErrAlreadyMember):
			respond.Error(w, http.StatusConflict, "conflict", "you are already a member of this room")
		default:
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not join room", err)
		}
		return
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "joined"})
}

func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	roomID, ok := pathUUID(w, r, "id", "room id")
	if !ok {
		return
	}
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}

	if err := h.repo.RemoveMember(r.Context(), roomID, userID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			respond.Error(w, http.StatusNotFound, "not_found", "room not found")
		case errors.Is(err, ErrNotMember):
			respond.Error(w, http.StatusNotFound, "not_found", "you are not a member of this room")
		default:
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not leave room", err)
		}
		return
	}
	if h.disc != nil {
		h.disc.Disconnect(roomID, userID)
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "left"})
}

type updateRoomRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	IsPrivate   *bool   `json:"is_private"`
}

// update edits a room. Admins may rename/describe; only the owner may change
// visibility.
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	roomID, _, role, ok := h.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	var req updateRoomRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Name == nil && req.Description == nil && req.IsPrivate == nil {
		respond.Error(w, http.StatusBadRequest, "validation", "nothing to update")
		return
	}
	if req.IsPrivate != nil && role != RoleOwner {
		respond.Error(w, http.StatusForbidden, "forbidden", "only the room owner can change room visibility")
		return
	}
	cur, err := h.repo.ByID(r.Context(), roomID)
	if err != nil {
		h.roomErr(w, err, "could not update room")
		return
	}
	name, desc := cur.Name, cur.Description
	if req.Name != nil {
		name = *req.Name
	}
	if req.Description != nil {
		desc = *req.Description
	}
	if err := validateRoom(name, desc); err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	room, err := h.repo.Update(r.Context(), roomID, UpdateParams{Name: req.Name, Description: req.Description, IsPrivate: req.IsPrivate})
	if err != nil {
		if errors.Is(err, ErrNameTaken) {
			respond.Error(w, http.StatusConflict, "conflict", "a room with that name already exists")
			return
		}
		h.roomErr(w, err, "could not update room")
		return
	}
	respond.JSON(w, http.StatusOK, room)
}

func (h *Handler) roomErr(w http.ResponseWriter, err error, msg string) {
	if errors.Is(err, ErrNotFound) {
		respond.Error(w, http.StatusNotFound, "not_found", "room not found")
		return
	}
	respond.Errorf(w, http.StatusInternalServerError, "internal", msg, err)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	roomID, _, _, ok := h.authorize(w, r, RoleOwner)
	if !ok {
		return
	}
	if err := h.repo.Delete(r.Context(), roomID); err != nil {
		h.roomErr(w, err, "could not delete room")
		return
	}
	if h.disc != nil {
		h.disc.CloseRoom(roomID)
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	roomID, userID, _, ok := h.authorize(w, r, RoleMember)
	if !ok {
		return
	}
	if err := h.repo.MarkRead(r.Context(), roomID, userID); err != nil {
		if errors.Is(err, ErrNotMember) {
			respond.Error(w, http.StatusNotFound, "not_found", "room not found or you are not a member")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not mark room read", err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "read"})
}

func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	roomID, _, _, ok := h.authorize(w, r, RoleMember)
	if !ok {
		return
	}
	members, err := h.repo.Members(r.Context(), roomID)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not list members", err)
		return
	}
	respond.JSON(w, http.StatusOK, members)
}

// kick removes another member. Owners can remove anyone; admins only plain
// members. You cannot kick yourself (use leave).
func (h *Handler) kick(w http.ResponseWriter, r *http.Request) {
	roomID, userID, role, ok := h.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	target, ok := pathUUID(w, r, "userID", "user id")
	if !ok {
		return
	}
	if target == userID {
		respond.Error(w, http.StatusBadRequest, "validation", "use leave to remove yourself")
		return
	}
	targetRole, err := h.repo.Role(r.Context(), roomID, target)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			respond.Error(w, http.StatusNotFound, "not_found", "that user is not a member of this room")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not remove member", err)
		return
	}
	if targetRole.rank() >= role.rank() {
		respond.Error(w, http.StatusForbidden, "forbidden", "you cannot remove a member of equal or higher rank")
		return
	}
	if err := h.repo.RemoveMember(r.Context(), roomID, target); err != nil {
		if errors.Is(err, ErrNotMember) || errors.Is(err, ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "that user is not a member of this room")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not remove member", err)
		return
	}
	if h.disc != nil {
		h.disc.Disconnect(roomID, target)
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

type setRoleRequest struct {
	Role MemberRole `json:"role"`
}

// setRole promotes a member to admin or demotes an admin. Owner only; the
// owner role itself is never granted here.
func (h *Handler) setRole(w http.ResponseWriter, r *http.Request) {
	roomID, userID, _, ok := h.authorize(w, r, RoleOwner)
	if !ok {
		return
	}
	target, ok := pathUUID(w, r, "userID", "user id")
	if !ok {
		return
	}
	var req setRoleRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Role != RoleAdmin && req.Role != RoleMember {
		respond.Error(w, http.StatusBadRequest, "validation", "role must be admin or member")
		return
	}
	if target == userID {
		respond.Error(w, http.StatusBadRequest, "validation", "you cannot change your own role")
		return
	}
	targetRole, err := h.repo.Role(r.Context(), roomID, target)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			respond.Error(w, http.StatusNotFound, "not_found", "that user is not a member of this room")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not change role", err)
		return
	}
	if targetRole == RoleOwner {
		respond.Error(w, http.StatusForbidden, "forbidden", "an owner's role cannot be changed")
		return
	}
	if err := h.repo.SetRole(r.Context(), roomID, target, req.Role); err != nil {
		if errors.Is(err, ErrNotMember) {
			respond.Error(w, http.StatusNotFound, "not_found", "that user is not a member of this room")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not change role", err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "updated", "role": string(req.Role)})
}

type createInviteRequest struct {
	MaxUses  int `json:"max_uses"`  // 0 = unlimited
	TTLHours int `json:"ttl_hours"` // 0 = default (7 days)
}

type createInviteResponse struct {
	Code   string `json:"code"`
	Invite Invite `json:"invite"`
}

func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) {
	roomID, userID, _, ok := h.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	var req createInviteRequest
	// An empty body means defaults.
	if r.ContentLength != 0 {
		if !decode(w, r, &req) {
			return
		}
	}
	if req.MaxUses < 0 || req.MaxUses > maxInviteMaxUses {
		respond.Error(w, http.StatusBadRequest, "validation", fmt.Sprintf("max_uses must be 0-%d", maxInviteMaxUses))
		return
	}
	ttl := defaultInviteTTL
	if req.TTLHours != 0 {
		ttl = time.Duration(req.TTLHours) * time.Hour
		if req.TTLHours < 0 || ttl > maxInviteTTL {
			respond.Error(w, http.StatusBadRequest, "validation",
				fmt.Sprintf("ttl_hours must be 1-%d", int(maxInviteTTL/time.Hour)))
			return
		}
	}

	code, err := newInviteCode()
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not create invite", err)
		return
	}
	inv, err := h.repo.CreateInvite(r.Context(), roomID, userID, HashInviteCode(code), h.now().Add(ttl), req.MaxUses)
	if err != nil {
		h.roomErr(w, err, "could not create invite")
		return
	}
	respond.JSON(w, http.StatusCreated, createInviteResponse{Code: code, Invite: inv})
}

func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request) {
	roomID, _, _, ok := h.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	invs, err := h.repo.ListInvites(r.Context(), roomID)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not list invites", err)
		return
	}
	respond.JSON(w, http.StatusOK, invs)
}

func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) {
	roomID, _, _, ok := h.authorize(w, r, RoleAdmin)
	if !ok {
		return
	}
	inviteID, ok := pathUUID(w, r, "inviteID", "invite id")
	if !ok {
		return
	}
	if err := h.repo.RevokeInvite(r.Context(), roomID, inviteID); err != nil {
		if errors.Is(err, ErrNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "invite not found")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not revoke invite", err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

type redeemInviteRequest struct {
	Code string `json:"code"`
}

func (h *Handler) redeemInvite(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req redeemInviteRequest
	if !decode(w, r, &req) {
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 128 {
		respond.Error(w, http.StatusBadRequest, "validation", "code is required")
		return
	}
	room, err := h.repo.RedeemInvite(r.Context(), HashInviteCode(code), userID)
	if err != nil {
		switch {
		case errors.Is(err, ErrInviteInvalid):
			respond.Error(w, http.StatusNotFound, "not_found", "invite is invalid, expired or used up")
		case errors.Is(err, ErrAlreadyMember):
			respond.Error(w, http.StatusConflict, "conflict", "you are already a member of this room")
		default:
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not redeem invite", err)
		}
		return
	}
	respond.JSON(w, http.StatusOK, room)
}

type openDMRequest struct {
	Username string `json:"username"`
}

func (h *Handler) openDM(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req openDMRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Username == "" || len(req.Username) > 64 {
		respond.Error(w, http.StatusBadRequest, "validation", "username is required")
		return
	}
	room, err := h.repo.OpenDM(r.Context(), userID, req.Username)
	if err != nil {
		switch {
		case errors.Is(err, ErrUserNotFound):
			respond.Error(w, http.StatusNotFound, "not_found", "user not found")
		case errors.Is(err, ErrSelfDM):
			respond.Error(w, http.StatusBadRequest, "validation", "you cannot message yourself")
		default:
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not open direct message", err)
		}
		return
	}
	respond.JSON(w, http.StatusOK, room)
}

// newInviteCode returns a fresh unguessable invite code (96 bits, base32).
func newInviteCode() (string, error) {
	buf := make([]byte, inviteCodeEntropy)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)), nil
}

// HashInviteCode returns the stored digest of an invite code. Codes are
// case-insensitive.
func HashInviteCode(code string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(code))))
	return hex.EncodeToString(sum[:])
}

// pathUUID parses a UUID path parameter, writing a 400 and returning false
// when it is invalid.
func pathUUID(w http.ResponseWriter, r *http.Request, param, label string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(param))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", label+" must be a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

// validateRoom enforces name and description bounds.
func validateRoom(name, description string) error {
	if len(name) < nameMinLen || len(name) > nameMaxLen {
		return fmt.Errorf("room name must be %d-%d characters", nameMinLen, nameMaxLen)
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("room name must not be blank")
	}
	if strings.HasPrefix(strings.ToLower(name), reservedNamePrefix) {
		return fmt.Errorf("room names starting with %q are reserved", reservedNamePrefix)
	}
	if strings.ContainsAny(name, "\r\n\t\x00\x1b") {
		return errors.New("room name must not contain control characters")
	}
	if len(description) > descMaxLen {
		return fmt.Errorf("room description must be at most %d characters", descMaxLen)
	}
	return nil
}
