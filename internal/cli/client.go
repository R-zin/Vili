package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// renewSkew is how long before the access token's expiry the client renews it
// proactively, so an in-flight command never races an expiring token.
const renewSkew = 30 * time.Second

// Client is a thin HTTP consumer of the Vili backend API. It carries the
// bearer token from the session and decodes the server's uniform error
// envelope into Go errors.
type Client struct {
	// BaseURL is the server root, e.g. http://localhost:8080 (no trailing slash).
	BaseURL string
	// Token is the JWT from login; empty until authenticated.
	Token string
	// RefreshToken renews Token when it expires; empty disables silent renewal.
	RefreshToken string
	// TokenExpiry is when Token expires (from the login/refresh response).
	TokenExpiry time.Time

	// Username is the signed-in user's name (from the saved session), used to
	// flag @mentions in rendered messages. Empty when unknown.
	Username string

	// UI is the presentation for this invocation (nil renders plain text).
	UI *UI
	// Busy, when set, is called as each request starts; the returned func is
	// called when it ends. The CLI uses it to show a spinner.
	Busy func() (done func())

	// OnTokenRefresh is called after a successful silent renewal so the caller
	// can persist the rotated token pair. Nil disables silent renewal.
	OnTokenRefresh func(access, refresh string, expiry time.Time)

	http *http.Client
	// renewMu serializes token renewal so a chat view and a send that both hit
	// a 401 (or an expired token) rotate only once, not twice — a double
	// rotation would trip the server's reuse detection and force a logout.
	renewMu sync.Mutex
}

// NewClient builds a Client with a sane request timeout.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Error is the decoded server error envelope, exposed so the CLI can print the
// server's human-safe message and branch on status codes.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func (e *Error) Error() string { return e.Message }

// do performs one JSON request and decodes the body into out (when out is
// non-nil). A non-2xx response is decoded from the uniform error envelope and
// returned as *Error. When the access token is expired (or about to be) and a
// refresh token is available, do renews it first; a single 401 also triggers
// one renew-and-retry, so a token that expired mid-session is transparently
// refreshed rather than surfacing a spurious auth error.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if err := c.ensureFreshToken(ctx); err != nil {
		return err
	}
	if err := c.doOnce(ctx, method, path, query, body, out); err != nil {
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
			return err
		}
		// The token was rejected even though it isn't near expiry (or is): renew
		// once, then replay. A failed renewal means the session is dead.
		if !c.canRefresh() {
			return err
		}
		if rerr := c.renewIfStale(ctx, c.Token); rerr != nil {
			return rerr
		}
		return c.doOnce(ctx, method, path, query, body, out)
	}
	return nil
}

// doOnce performs a single request attempt with the client's current token.
func (c *Client) doOnce(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if c.Busy != nil {
		done := c.Busy()
		defer done()
	}
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var env struct {
			Error Error `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return fmt.Errorf("%s %s: status %d", method, path, resp.StatusCode)
		}
		env.Error.Status = resp.StatusCode
		return &env.Error
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// ensureFreshToken proactively renews the access token when it is expired or
// within the renew skew window, provided a refresh token and callback are
// configured.
func (c *Client) ensureFreshToken(ctx context.Context) error {
	if !c.canRefresh() || time.Now().Add(renewSkew).Before(c.TokenExpiry) {
		return nil
	}
	return c.renewIfStale(ctx, c.Token)
}

// canRefresh reports whether silent renewal is configured.
func (c *Client) canRefresh() bool {
	return c.RefreshToken != "" && c.OnTokenRefresh != nil
}

// renewIfStale exchanges the refresh token for a new access+refresh pair,
// single-flight. staleToken is the access token the caller was about to use (or
// that was just rejected): if another goroutine already renewed while we waited
// on the mutex, the current Token differs and we skip the redundant rotation —
// two rotations would trip the server's reuse detection and force a logout. A
// refresh rejected by the server means the session is dead and the user must
// log in again.
func (c *Client) renewIfStale(ctx context.Context, staleToken string) error {
	c.renewMu.Lock()
	defer c.renewMu.Unlock()

	if c.Token != staleToken {
		return nil // someone already renewed
	}

	var resp loginResponse
	err := c.doOnce(ctx, http.MethodPost, "/v1/auth/refresh", nil,
		map[string]string{"refresh_token": c.RefreshToken}, &resp)
	if err != nil {
		return fmt.Errorf("session expired — run: vili login (%w)", err)
	}
	c.applySession(resp)
	return nil
}

// applySession stores a fresh token pair on the client and persists it via the
// callback.
func (c *Client) applySession(resp loginResponse) {
	c.Token = resp.Token
	c.RefreshToken = resp.RefreshToken
	c.TokenExpiry = resp.ExpiresAt
	if c.OnTokenRefresh != nil {
		c.OnTokenRefresh(resp.Token, resp.RefreshToken, resp.ExpiresAt)
	}
}

// esc escapes a value for use as a single URL path segment, so a user-supplied
// id like "../x" cannot rewrite the request path.
func esc(s string) string { return url.PathEscape(s) }

// credentials is the shared register/login request body.
type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Register creates an account and returns the safe user.
func (c *Client) Register(ctx context.Context, username, password string) (User, error) {
	var u User
	err := c.do(ctx, http.MethodPost, "/v1/auth/register", nil, credentials{username, password}, &u)
	return u, err
}

// Login authenticates and returns the full session payload (access token,
// refresh token, expiry, and user). It bypasses renewal since the caller is
// unauthenticated.
func (c *Client) Login(ctx context.Context, username, password string) (loginResponse, error) {
	var resp loginResponse
	err := c.doOnce(ctx, http.MethodPost, "/v1/auth/login", nil, credentials{username, password}, &resp)
	return resp, err
}

// Logout revokes the refresh token server-side (best-effort) so the session
// cannot be renewed after the local session file is cleared.
func (c *Client) Logout(ctx context.Context) error {
	if c.RefreshToken == "" {
		return nil
	}
	return c.doOnce(ctx, http.MethodPost, "/v1/auth/logout", nil,
		map[string]string{"refresh_token": c.RefreshToken}, nil)
}

// CreateRoom creates a public room owned by the authenticated user.
func (c *Client) CreateRoom(ctx context.Context, name, description string) (Room, error) {
	return c.CreateRoomPrivate(ctx, name, description, false)
}

// CreateRoomPrivate creates a room, optionally private (invite-only).
func (c *Client) CreateRoomPrivate(ctx context.Context, name, description string, private bool) (Room, error) {
	var r Room
	body := map[string]any{"name": name, "description": description, "is_private": private}
	err := c.do(ctx, http.MethodPost, "/v1/createroom", nil, body, &r)
	return r, err
}

// ListRooms returns the rooms visible to the user, newest first.
func (c *Client) ListRooms(ctx context.Context) ([]Room, error) {
	return c.ListRoomsFiltered(ctx, "", false)
}

// ListRoomsFiltered lists rooms matching a name query, optionally only those
// the user belongs to. Results carry the user's role and unread counters.
func (c *Client) ListRoomsFiltered(ctx context.Context, query string, mine bool) ([]Room, error) {
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if mine {
		q.Set("mine", "1")
	}
	var rooms []Room
	err := c.do(ctx, http.MethodGet, "/v1/listrooms", q, nil, &rooms)
	return rooms, err
}

// MarkRead advances the user's read cursor in a room to now.
func (c *Client) MarkRead(ctx context.Context, roomID string) error {
	return c.do(ctx, http.MethodPost, "/v1/rooms/"+esc(roomID)+"/read", nil, nil, &statusResponse{})
}

// Members lists a room's members.
func (c *Client) Members(ctx context.Context, roomID string) ([]Member, error) {
	var ms []Member
	err := c.do(ctx, http.MethodGet, "/v1/rooms/"+esc(roomID)+"/members", nil, nil, &ms)
	return ms, err
}

// Kick removes a member from a room (owner/admin).
func (c *Client) Kick(ctx context.Context, roomID, userID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/rooms/"+esc(roomID)+"/members/"+esc(userID), nil, nil, &statusResponse{})
}

// SetRole promotes/demotes a member (owner only): role is admin or member.
func (c *Client) SetRole(ctx context.Context, roomID, userID, role string) error {
	return c.do(ctx, http.MethodPut, "/v1/rooms/"+esc(roomID)+"/members/"+esc(userID)+"/role", nil,
		map[string]string{"role": role}, &statusResponse{})
}

// RoomUpdate carries the optional fields of a room edit; nil is "unchanged".
type RoomUpdate struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	IsPrivate   *bool   `json:"is_private,omitempty"`
}

// UpdateRoom edits a room.
func (c *Client) UpdateRoom(ctx context.Context, roomID string, u RoomUpdate) (Room, error) {
	var r Room
	err := c.do(ctx, http.MethodPatch, "/v1/rooms/"+esc(roomID), nil, u, &r)
	return r, err
}

// DeleteRoom deletes a room (owner only).
func (c *Client) DeleteRoom(ctx context.Context, roomID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/rooms/"+esc(roomID), nil, nil, &statusResponse{})
}

// CreateInvite mints an invite code; maxUses 0 and ttlHours 0 mean the server
// defaults (unlimited uses, 7 days).
func (c *Client) CreateInvite(ctx context.Context, roomID string, maxUses, ttlHours int) (string, Invite, error) {
	var resp createInviteResponse
	err := c.do(ctx, http.MethodPost, "/v1/rooms/"+esc(roomID)+"/invites", nil,
		map[string]int{"max_uses": maxUses, "ttl_hours": ttlHours}, &resp)
	return resp.Code, resp.Invite, err
}

// ListInvites lists a room's invites.
func (c *Client) ListInvites(ctx context.Context, roomID string) ([]Invite, error) {
	var invs []Invite
	err := c.do(ctx, http.MethodGet, "/v1/rooms/"+esc(roomID)+"/invites", nil, nil, &invs)
	return invs, err
}

// RevokeInvite revokes an invite.
func (c *Client) RevokeInvite(ctx context.Context, roomID, inviteID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/rooms/"+esc(roomID)+"/invites/"+esc(inviteID), nil, nil, &statusResponse{})
}

// RedeemInvite joins the room an invite code belongs to.
func (c *Client) RedeemInvite(ctx context.Context, code string) (Room, error) {
	var r Room
	err := c.do(ctx, http.MethodPost, "/v1/invites/redeem", nil, map[string]string{"code": code}, &r)
	return r, err
}

// OpenDM finds or creates a direct-message room with another user.
func (c *Client) OpenDM(ctx context.Context, username string) (Room, error) {
	var r Room
	err := c.do(ctx, http.MethodPost, "/v1/dms", nil, map[string]string{"username": username}, &r)
	return r, err
}

// Join adds the authenticated user to the room as a member.
func (c *Client) Join(ctx context.Context, roomID string) error {
	return c.do(ctx, http.MethodPost, "/v1/rooms/"+esc(roomID)+"/join", nil, nil, &statusResponse{})
}

// Leave removes the authenticated user from the room.
func (c *Client) Leave(ctx context.Context, roomID string) error {
	return c.do(ctx, http.MethodPost, "/v1/rooms/"+esc(roomID)+"/leave", nil, nil, &statusResponse{})
}

// History fetches up to limit messages for a room, oldest first. before
// optionally bounds the fetch to messages older than the cursor; nil means
// "most recent".
func (c *Client) History(ctx context.Context, roomID string, limit int, before *time.Time) ([]Message, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	if before != nil {
		q.Set("before", before.UTC().Format(time.RFC3339Nano))
	}
	var messages []Message
	err := c.do(ctx, http.MethodGet, "/v1/rooms/"+esc(roomID)+"/messages", q, nil, &messages)
	return messages, err
}

// SendMessage posts content to a room as the authenticated user.
func (c *Client) SendMessage(ctx context.Context, roomID, content, typ string) (Message, error) {
	return c.SendReply(ctx, roomID, content, typ, "")
}

// SendReply posts content as a reply to another message (replyTo may be empty).
func (c *Client) SendReply(ctx context.Context, roomID, content, typ, replyTo string) (Message, error) {
	var m Message
	body := map[string]string{"content": content}
	if typ != "" {
		body["type"] = typ
	}
	if replyTo != "" {
		body["reply_to"] = replyTo
	}
	err := c.do(ctx, http.MethodPost, "/v1/rooms/"+esc(roomID)+"/messages", nil, body, &m)
	return m, err
}

// EditMessage replaces the content of one of the user's own messages.
func (c *Client) EditMessage(ctx context.Context, roomID, msgID, content string) (Message, error) {
	var m Message
	err := c.do(ctx, http.MethodPatch, "/v1/rooms/"+esc(roomID)+"/messages/"+esc(msgID), nil,
		map[string]string{"content": content}, &m)
	return m, err
}

// DeleteMessage deletes a message (own, or any as room owner/admin).
func (c *Client) DeleteMessage(ctx context.Context, roomID, msgID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/rooms/"+esc(roomID)+"/messages/"+esc(msgID), nil, nil, &statusResponse{})
}

// Search runs a full-text search over the user's rooms (roomID optional).
func (c *Client) Search(ctx context.Context, query, roomID string, limit int) ([]Message, error) {
	q := url.Values{"q": {query}}
	if roomID != "" {
		q.Set("room_id", roomID)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	var ms []Message
	err := c.do(ctx, http.MethodGet, "/v1/search", q, nil, &ms)
	return ms, err
}

// Me returns the authenticated user's profile.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/v1/me", nil, nil, &u)
	return u, err
}

// ChangePassword sets a new password and returns the fresh session (every
// other session is signed out by the server).
func (c *Client) ChangePassword(ctx context.Context, current, next string) (loginResponse, error) {
	var resp loginResponse
	err := c.do(ctx, http.MethodPost, "/v1/me/password", nil,
		map[string]string{"current_password": current, "new_password": next}, &resp)
	return resp, err
}

// DeleteAccount permanently deletes the account.
func (c *Client) DeleteAccount(ctx context.Context, password string) error {
	return c.do(ctx, http.MethodDelete, "/v1/me", nil, map[string]string{"password": password}, &statusResponse{})
}

// ListSessions lists the user's signed-in sessions.
func (c *Client) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	var ss []SessionInfo
	err := c.do(ctx, http.MethodGet, "/v1/me/sessions", nil, nil, &ss)
	return ss, err
}

// RevokeSession signs out one session; id "" signs out all of them.
func (c *Client) RevokeSession(ctx context.Context, id string) error {
	path := "/v1/me/sessions"
	if id != "" {
		path += "/" + esc(id)
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil, &statusResponse{})
}
