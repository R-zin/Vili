package room

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/respond"
)

// Validation bounds for room fields.
const (
	nameMinLen = 1
	nameMaxLen = 120
	descMaxLen = 500
)

// Handler serves the room feature's routes. All of them are protected; the
// wiring layer mounts them behind the auth middleware, so a user id is always
// present in the request context.
type Handler struct {
	repo Repository
}

// NewHandler builds a room Handler.
func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

// RegisterRoutes mounts the protected room routes on the mux, each wrapped in
// the auth middleware provided by require.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, require func(http.HandlerFunc) http.Handler) {
	mux.Handle("POST /v1/createroom", require(h.create))
	mux.Handle("GET /v1/listrooms", require(h.list))
	mux.Handle("POST /v1/rooms/{id}/join", require(h.join))
	mux.Handle("POST /v1/rooms/{id}/leave", require(h.leave))
	// Realtime: GET /v1/rooms/{id}/ws is a Phase 4 websocket route and is
	// intentionally not registered in Phase 1.
}

type createRoomRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}

	var req createRoomRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if err := validateRoom(req.Name, req.Description); err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}

	room, err := h.repo.Create(r.Context(), userID, req.Name, req.Description)
	if err != nil {
		if errors.Is(err, ErrNameTaken) {
			respond.Error(w, http.StatusConflict, "conflict", "a room with that name already exists")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not create room", err)
		return
	}

	// The creator becomes the room owner. If this fails we still created the
	// room; log it but report success of the primary action.
	if err := h.repo.AddMember(r.Context(), room.ID, userID, RoleOwner); err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not finalize room ownership", err)
		return
	}

	respond.JSON(w, http.StatusCreated, room)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserIDFromContext(r.Context()); !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}

	rooms, err := h.repo.List(r.Context())
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not list rooms", err)
		return
	}
	respond.JSON(w, http.StatusOK, rooms)
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	roomID, ok := pathRoomID(w, r)
	if !ok {
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
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
	roomID, ok := pathRoomID(w, r)
	if !ok {
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
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
	respond.JSON(w, http.StatusOK, map[string]string{"status": "left"})
}

// pathRoomID parses the {id} path parameter as a UUID, writing a 400 response
// and returning false when it is invalid.
func pathRoomID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "room id must be a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

// validateRoom enforces name and description bounds.
func validateRoom(name, description string) error {
	if len(name) < nameMinLen || len(name) > nameMaxLen {
		return fmt.Errorf("room name must be %d-%d characters", nameMinLen, nameMaxLen)
	}
	if len(description) > descMaxLen {
		return fmt.Errorf("room description must be at most %d characters", descMaxLen)
	}
	return nil
}
