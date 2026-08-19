package ws

import (
	"net/http"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/respond"
)

// Handler serves the realtime websocket route. It is protected: the wiring
// layer mounts it behind the auth middleware, and it enforces room membership
// before upgrading, exactly as REST history/posts are gated.
type Handler struct {
	hub        *Hub
	membership MembershipChecker
	usernames  UsernameResolver
	// originPatterns authorizes browser cross-origin connect (e.g. the local
	// index.html demo). The request host and header-less clients (the CLI) are
	// always allowed.
	originPatterns []string
}

// NewHandler builds a Handler. opts are optional OriginPatterns entries used
// to permit browser cross-origin websockets (hostname[:port] patterns).
func NewHandler(hub *Hub, membership MembershipChecker, usernames UsernameResolver, opts ...string) *Handler {
	return &Handler{hub: hub, membership: membership, usernames: usernames, originPatterns: opts}
}

// RegisterRoutes mounts the protected websocket route on the mux. requireWS is
// the websocket-capable auth wrapper (auth.TokenService.RequireWS) that also
// honors a ?token= query for browser clients, which cannot set Authorization.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, requireWS func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /v1/rooms/{id}/ws", requireWS(h.serveWS))
}

// serveWS upgrades a room member's connection to a websocket and runs it.
// Before the upgrade it behaves like any protected REST handler — parse the
// room id, read the authenticated user, enforce membership — so failures get
// the standard error envelope (404 for non-members so membership isn't
// enumerable). Only after those checks pass does it hijack the connection.
func (h *Handler) serveWS(w http.ResponseWriter, r *http.Request) {
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

	member, err := h.membership.IsMember(r.Context(), roomID, userID)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not open connection", err)
		return
	}
	if !member {
		respond.Error(w, http.StatusNotFound, "not_found", "room not found or you are not a member")
		return
	}

	username, err := h.usernames.UsernameByID(r.Context(), userID)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not open connection", err)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.originPatterns})
	if err != nil {
		// Accept already wrote the failure response; nothing more to send.
		return
	}

	client := newClient(h.hub, conn, roomID, userID, username)
	// run owns the connection and blocks until it ends; keep the handler alive
	// for the life of the socket.
	client.run(r.Context())
}
