package message

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
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

// Handler serves the message feature's routes. They are protected; the wiring
// layer mounts them behind the auth middleware.
type Handler struct {
	repo Repository
}

// NewHandler builds a message Handler.
func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

// RegisterRoutes mounts the protected message routes on the mux, each wrapped
// in the auth middleware provided by require.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, require func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /v1/rooms/{id}/messages", require(h.list))
	mux.Handle("POST /v1/rooms/{id}/messages", require(h.create))
}

// createMessageRequest is the body for posting a message. Type is optional and
// defaults to text.
type createMessageRequest struct {
	Content string      `json:"content"`
	Type    MessageType `json:"type"`
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

	msg := &Message{RoomID: roomID, UserID: userID, Content: req.Content, Type: req.Type}
	if err := h.repo.Create(r.Context(), msg); err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "room not found")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not post message", err)
		return
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
