package user

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/respond"
)

// Validation bounds for credentials.
const (
	usernameMinLen = 1
	usernameMaxLen = 64
	passwordMinLen = 8
)

// Handler serves the user feature's routes (register, login, refresh, logout).
// It depends on the narrow Repository interface, a TokenService, and a refresh
// store, all injected.
type Handler struct {
	repo       Repository
	tokens     *auth.TokenService
	refresh    auth.RefreshStore
	refreshTTL time.Duration
}

// NewHandler builds a user Handler. refreshTTL is how long a refresh token
// remains valid; refresh may be nil only when refresh/logout are not served
// (tests of register/login alone).
func NewHandler(repo Repository, tokens *auth.TokenService, refresh auth.RefreshStore, refreshTTL time.Duration) *Handler {
	return &Handler{repo: repo, tokens: tokens, refresh: refresh, refreshTTL: refreshTTL}
}

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token        string    `json:"token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	User         Public    `json:"user"`
}

// refreshRequest is the body for refresh and logout: the refresh token is the
// credential in both cases (the access token may already be expired).
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// RegisterRoutes mounts the public auth routes on the mux. wrap applies to all
// of them (the wiring layer passes its per-IP rate limiter, or nil to skip).
func (h *Handler) RegisterRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/auth/register", apply(wrap, h.register))
	mux.Handle("POST /v1/auth/login", apply(wrap, h.login))
	mux.Handle("POST /v1/auth/refresh", apply(wrap, h.refreshHandler))
	mux.Handle("POST /v1/auth/logout", apply(wrap, h.logout))
}

// apply wraps fn with the middleware, or returns fn untouched when mw is nil.
func apply(mw func(http.Handler) http.Handler, fn http.HandlerFunc) http.Handler {
	if mw == nil {
		return fn
	}
	return mw(fn)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if err := validateCredentials(req.Username, req.Password, passwordMinLen); err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not create user", err)
		return
	}

	u, err := h.repo.Create(r.Context(), req.Username, hash)
	if err != nil {
		if errors.Is(err, ErrUsernameTaken) {
			respond.Error(w, http.StatusConflict, "conflict", "username is already taken")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not create user", err)
		return
	}

	respond.JSON(w, http.StatusCreated, u.Public())
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	// On login the password only needs to be non-empty; any credential
	// mismatch below returns the same generic 401.
	if err := validateCredentials(req.Username, req.Password, 1); err != nil {
		respond.Error(w, http.StatusBadRequest, "validation", err.Error())
		return
	}

	u, err := h.repo.ByUsername(r.Context(), req.Username)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Generic message: do not reveal whether the user exists.
			respond.Error(w, http.StatusUnauthorized, "unauthorized", "invalid credentials")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not sign in", err)
		return
	}

	if err := auth.CompareHashAndPassword(u.PasswordHash, req.Password); err != nil {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "invalid credentials")
		return
	}

	// Login starts a new token family: issue an access token and a fresh
	// refresh token bound to a new family id.
	resp, _, err := h.mintPair(r.Context(), u, uuid.New())
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not sign in", err)
		return
	}

	respond.JSON(w, http.StatusOK, resp)
}

// refreshHandler exchanges a valid refresh token for a new access token and a
// rotated refresh token. Rotation is single-use: presenting an already-used
// token means it was stolen and replayed, so the whole family is revoked.
func (h *Handler) refreshHandler(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if req.RefreshToken == "" {
		respond.Error(w, http.StatusBadRequest, "validation", "refresh_token is required")
		return
	}

	rt, err := h.refresh.RefreshTokenByHash(r.Context(), auth.HashRefresh(req.RefreshToken))
	if err != nil {
		if errors.Is(err, auth.ErrRefreshNotFound) {
			respond.Error(w, http.StatusUnauthorized, "unauthorized", "invalid refresh token")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not refresh", err)
		return
	}

	// Reuse of a consumed token indicates theft: revoke the entire family so the
	// real client and the attacker are both forced to re-login.
	if rt.Consumed {
		if err := h.refresh.RevokeRefreshFamily(r.Context(), rt.FamilyID); err != nil {
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not refresh", err)
			return
		}
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "refresh token reuse detected")
		return
	}
	if time.Now().After(rt.ExpiresAt) {
		respond.Error(w, http.StatusUnauthorized, "unauthorized", "refresh token expired")
		return
	}

	u, err := h.repo.ByID(r.Context(), rt.UserID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			respond.Error(w, http.StatusUnauthorized, "unauthorized", "invalid refresh token")
			return
		}
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not refresh", err)
		return
	}

	// Rotate: mint a new pair in the same family, then mark the presented token
	// consumed and link it to its replacement.
	resp, newID, err := h.mintPair(r.Context(), u, rt.FamilyID)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not refresh", err)
		return
	}
	if err := h.refresh.ConsumeRefreshToken(r.Context(), rt.ID, newID); err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not refresh", err)
		return
	}

	respond.JSON(w, http.StatusOK, resp)
}

// logout revokes the presented refresh token. It is idempotent: an unknown
// token still returns 200 so logout never leaks whether a token exists.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.Error(w, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if req.RefreshToken != "" {
		if err := h.refresh.RevokeRefreshToken(r.Context(), auth.HashRefresh(req.RefreshToken)); err != nil {
			respond.Errorf(w, http.StatusInternalServerError, "internal", "could not log out", err)
			return
		}
	}
	respond.JSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// mintPair issues an access token for u and stores a new refresh token in
// family, returning the response body and the new refresh token's id (so the
// caller can mark the rotated-from token consumed). ExpiresAt is the access
// token's expiry, which the CLI uses to renew proactively.
func (h *Handler) mintPair(ctx context.Context, u User, family uuid.UUID) (loginResponse, uuid.UUID, error) {
	access, err := h.tokens.Issue(u.ID)
	if err != nil {
		return loginResponse{}, uuid.Nil, err
	}
	raw, hash, err := auth.GenerateRefresh()
	if err != nil {
		return loginResponse{}, uuid.Nil, err
	}
	now := time.Now()
	id, err := h.refresh.InsertRefreshToken(ctx, u.ID, family, hash, now.Add(h.refreshTTL))
	if err != nil {
		return loginResponse{}, uuid.Nil, err
	}
	return loginResponse{
		Token:        access,
		RefreshToken: raw,
		ExpiresAt:    now.Add(h.tokens.Expiry()),
		User:         u.Public(),
	}, id, nil
}

// validateCredentials enforces username length and a minimum password length.
func validateCredentials(username, password string, minPasswordLen int) error {
	if len(username) < usernameMinLen || len(username) > usernameMaxLen {
		return fmt.Errorf("username must be %d-%d characters", usernameMinLen, usernameMaxLen)
	}
	if len(password) < minPasswordLen {
		if minPasswordLen <= 1 {
			return fmt.Errorf("password must not be empty")
		}
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	return nil
}
