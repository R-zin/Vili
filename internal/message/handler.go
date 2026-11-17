package message

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/event"
	"github.com/R-zin/vili/internal/respond"
)

// Bounds for the limit query parameter.
const (
	defaultLimit = 50
	minLimit     = 1
	maxLimit     = 100
)

// Bounds for message content.
const (
	contentMinLen = 1
	contentMaxLen = 4000
)

// Publisher broadcasts a freshly-posted message to a room's live websocket
// connections. It lets the message feature publish without importing the ws
// feature (features never import each other); the wiring layer satisfies it
// with the realtime hub, and a nil publisher disables broadcasting (REST-only
// Phase-3 deployments).
type Publisher interface {
	Broadcast(roomID uuid.UUID, e event.Event)
}

// Handler serves the message feature's routes. They are protected; the wiring
// layer mounts them behind the auth middleware.
type Handler struct {
	repo      Repository
	publisher Publisher
}

// NewHandler builds a message Handler. publisher may be nil, in which case
// posting only persists (no realtime broadcast).
func NewHandler(repo Repository, publisher Publisher) *Handler {
	return &Handler{repo: repo, publisher: publisher}
}

// RegisterRoutes mounts the protected message routes on the mux, each wrapped
// in the auth middleware provided by require. The create route additionally
// applies the per-user rate-limiter mw INSIDE the auth wrapper (require(mw)),
// so the limiter keys on the authenticated user id that require stores in the
// context. mw may be nil to skip limiting. The read route is left unthrottled
// so scrolling history isn't capped.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, require func(http.HandlerFunc) http.Handler, mw func(http.HandlerFunc) http.HandlerFunc) {
	mux.Handle("GET /v1/rooms/{id}/messages", require(h.list))
	mux.Handle("GET /v1/search", require(h.search))
	create, edit := h.create, h.edit
	if mw != nil {
		create, edit = mw(create), mw(edit)
	}
	mux.Handle("POST /v1/rooms/{id}/messages", require(create))
	mux.Handle("PATCH /v1/rooms/{id}/messages/{msgID}", require(edit))
	mux.Handle("DELETE /v1/rooms/{id}/messages/{msgID}", require(h.delete))
}

// createMessageRequest is the body for posting a message. Type is optional and
// defaults to text.
type createMessageRequest struct {
	Content string      `json:"content"`
	Type    MessageType `json:"type"`
	ReplyTo *uuid.UUID  `json:"reply_to"`
}

// mentionRE finds @username tokens that are not part of a longer word or
// address (so "a@b.com" is not a mention).
var mentionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_@.])@([A-Za-z0-9_][A-Za-z0-9_.-]{0,63})`)

// maxMentions bounds how many distinct mentions one message can carry, so a
// message cannot be used to ping an entire room.
const maxMentions = 10

// extractMentions returns the distinct @usernames in content (without the @),
// in order of appearance, capped at maxMentions. Trailing sentence
// punctuation is not part of a name. Candidates are validated against room
// membership by the repository.
func extractMentions(content string) []string {
	matches := mentionRE.FindAllStringSubmatch(content, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := strings.TrimRight(m[1], ".-")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) == maxMentions {
			break
		}
	}
	return out
}

// create posts a message to a room. Only members may post; a non-member (or an
// unknown room) yields the same 404 as list so membership isn't enumerable.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	roomID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "room id must be a valid UUID")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}

	var req createMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if err := validateMessage(req.Content, req.Type); err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}

	member, err := h.repo.IsMember(r.Context(), roomID, userID)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not post message", err)
		return
	}
	if !member {
		respond.Error(w, http.StatusNotFound, "not_found", "room not found or you are not a member")
		return
	}

	msg := &Message{RoomID: roomID, UserID: userID, Content: req.Content, Type: req.Type,
		ReplyTo: req.ReplyTo, Mentions: extractMentions(req.Content)}
	if err := h.repo.Create(r.Context(), msg); err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "room not found")
			return
		}
		if errors.Is(err, ErrReplyNotFound) {
			respond.Error(w, http.StatusBadRequest, "validation", "reply_to must be a message in this room")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not post message", err)
		return
	}

	// Publish the persisted message to the room's live connections. REST is the
	// source of truth: the row is already stored, and a broadcast failure must
	// not fail the post, so it is logged, not returned.
	if h.publisher != nil {
		if e, err := event.NewMessage(roomID, msg); err != nil {
			slog.Error("could not encode message broadcast", "error", err)
		} else {
			h.publisher.Broadcast(roomID, e)
		}
	}

	respond.JSON(w, http.StatusCreated, msg)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	roomID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "room id must be a valid UUID")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}

	var before *time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			respond.Error(w, http.StatusBadRequest, "validation", "before must be an RFC3339 timestamp")
			return
		}
		before = &t
	}

	// History is members-only; a non-member gets the same 404 as an unknown
	// room so membership isn't enumerable.
	member, err := h.repo.IsMember(r.Context(), roomID, userID)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not list messages", err)
		return
	}
	if !member {
		respond.Error(w, http.StatusNotFound, "not_found", "room not found or you are not a member")
		return
	}

	messages, err := h.repo.ListByRoom(r.Context(), roomID, limit, before)
	if err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "room not found")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not list messages", err)
		return
	}

	respond.JSON(w, http.StatusOK, messages)
}

// editMessageRequest is the body for editing a message's content.
type editMessageRequest struct {
	Content string `json:"content"`
}

// loadMessage resolves {id}/{msgID}, the caller, and the caller's role, then
// loads the message, enforcing that it belongs to the room in the path. Any
// failure has already been written to w. A non-member sees the generic 404.
func (h *Handler) loadMessage(w http.ResponseWriter, r *http.Request) (msg Message, userID uuid.UUID, role string, ok bool) {
	roomID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "room id must be a valid UUID")
		return
	}
	msgID, err := uuid.Parse(r.PathValue("msgID"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "message id must be a valid UUID")
		return
	}
	userID, authed := auth.UserIDFromContext(r.Context())
	if !authed {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	role, err = h.repo.MemberRole(r.Context(), roomID, userID)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			respond.Error(w, http.StatusNotFound, "not_found", "room not found or you are not a member")
		} else {
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not load message", err)
		}
		return
	}
	msg, err = h.repo.ByID(r.Context(), msgID)
	if err != nil || msg.RoomID != roomID {
		if err == nil || errors.Is(err, ErrMessageNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "message not found")
		} else {
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not load message", err)
		}
		return
	}
	return msg, userID, role, true
}

// edit changes the content of the caller's own live message.
func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	msg, userID, _, ok := h.loadMessage(w, r)
	if !ok {
		return
	}
	var req editMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if err := validateMessage(req.Content, ""); err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	if msg.Deleted {
		respond.Error(w, http.StatusNotFound, "not_found", "message not found")
		return
	}
	if msg.UserID != userID {
		respond.Error(w, http.StatusForbidden, "forbidden", "you can only edit your own messages")
		return
	}

	updated, err := h.repo.Edit(r.Context(), msg.ID, req.Content, extractMentions(req.Content))
	if err != nil {
		if errors.Is(err, ErrMessageNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "message not found")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not edit message", err)
		return
	}
	if h.publisher != nil {
		if e, err := event.NewMessageEdited(updated.RoomID, updated); err != nil {
			slog.Error("could not encode edit broadcast", "error", err)
		} else {
			h.publisher.Broadcast(updated.RoomID, e)
		}
	}
	respond.JSON(w, http.StatusOK, updated)
}

// delete tombstones a message. Authors may delete their own; room owners and
// admins may delete anyone's (moderation).
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	msg, userID, role, ok := h.loadMessage(w, r)
	if !ok {
		return
	}
	if msg.UserID != userID && role != "owner" && role != "admin" {
		respond.Error(w, http.StatusForbidden, "forbidden", "you can only delete your own messages")
		return
	}
	if err := h.repo.Delete(r.Context(), msg.ID); err != nil {
		if errors.Is(err, ErrMessageNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "message not found")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not delete message", err)
		return
	}
	if h.publisher != nil {
		h.publisher.Broadcast(msg.RoomID, event.NewMessageDeleted(msg.RoomID, msg.ID))
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// search runs a full-text query over rooms the caller belongs to.
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > maxSearchQueryLen {
		respond.Error(w, http.StatusBadRequest, "validation", fmt.Sprintf("q must be 1-%d characters", maxSearchQueryLen))
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}
	var roomID *uuid.UUID
	if raw := r.URL.Query().Get("room_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			respond.Error(w, http.StatusBadRequest, "validation", "room_id must be a valid UUID")
			return
		}
		roomID = &id
	}
	results, err := h.repo.Search(r.Context(), userID, q, roomID, limit)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not search", err)
		return
	}
	respond.JSON(w, http.StatusOK, results)
}

// maxSearchQueryLen bounds a search query.
const maxSearchQueryLen = 200

// validateMessage enforces content length bounds and a supported message type,
// defaulting an empty type to text.
func validateMessage(content string, typ MessageType) error {
	if len(content) < contentMinLen || len(content) > contentMaxLen {
		return fmt.Errorf("content must be %d-%d characters", contentMinLen, contentMaxLen)
	}
	switch typ {
	case "", TypeText, TypeDiff, TypeCode, TypeLog, TypeCommit:
		return nil
	default:
		return fmt.Errorf("type must be one of text, diff, code, log, commit")
	}
}

// parseLimit applies the default and clamps the limit into [min, max]. An
// unparsable value is a validation error.
func parseLimit(raw string) (int, error) {
	if raw == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("limit must be an integer")
	}
	if n < minLimit {
		return minLimit, nil
	}
	if n > maxLimit {
		return maxLimit, nil
	}
	return n, nil
}
