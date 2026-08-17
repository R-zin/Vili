package user

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/respond"
)

// Validation bounds for credentials.
const (
	usernameMinLen = 1
	usernameMaxLen = 64
	passwordMinLen = 8
)

// Handler serves the user feature's routes (register and login). It depends
// on the narrow Repository interface and a TokenService, both injected.
type Handler struct {
	repo   Repository
	tokens *auth.TokenService
}

// NewHandler builds a user Handler.
func NewHandler(repo Repository, tokens *auth.TokenService) *Handler {
	return &Handler{repo: repo, tokens: tokens}
}

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
	User  Public `json:"user"`
}

// RegisterRoutes mounts the public auth routes on the mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/register", h.register)
	mux.HandleFunc("POST /v1/auth/login", h.login)
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

	token, err := h.tokens.Issue(u.ID)
	if err != nil {
		respond.Errorf(w, http.StatusInternalServerError, "internal", "could not sign in", err)
		return
	}

	respond.JSON(w, http.StatusOK, loginResponse{Token: token, User: u.Public()})
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
