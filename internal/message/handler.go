package message

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/respond"
)

// Bounds for the limit query parameter.
const (
	defaultLimit = 50
	minLimit     = 1
	maxLimit     = 100
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
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	roomID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "room id must be a valid UUID")
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
