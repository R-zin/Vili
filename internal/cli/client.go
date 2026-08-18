package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin HTTP consumer of the Vili backend API. It carries the
// bearer token from the session and decodes the server's uniform error
// envelope into Go errors.
type Client struct {
	// BaseURL is the server root, e.g. http://localhost:8080 (no trailing slash).
	BaseURL string
	// Token is the JWT from login; empty until authenticated.
	Token string

	http *http.Client
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
// returned as *Error.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
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

// Login authenticates and returns the token and user.
func (c *Client) Login(ctx context.Context, username, password string) (string, User, error) {
	var resp loginResponse
	err := c.do(ctx, http.MethodPost, "/v1/auth/login", nil, credentials{username, password}, &resp)
	return resp.Token, resp.User, err
}

// CreateRoom creates a room owned by the authenticated user.
func (c *Client) CreateRoom(ctx context.Context, name, description string) (Room, error) {
	var r Room
	body := map[string]string{"name": name, "description": description}
	err := c.do(ctx, http.MethodPost, "/v1/createroom", nil, body, &r)
	return r, err
}

// ListRooms returns all rooms, newest first.
func (c *Client) ListRooms(ctx context.Context) ([]Room, error) {
	var rooms []Room
	err := c.do(ctx, http.MethodGet, "/v1/listrooms", nil, nil, &rooms)
	return rooms, err
}

// Join adds the authenticated user to the room as a member.
func (c *Client) Join(ctx context.Context, roomID string) error {
	return c.do(ctx, http.MethodPost, "/v1/rooms/"+roomID+"/join", nil, nil, &statusResponse{})
}

// Leave removes the authenticated user from the room.
func (c *Client) Leave(ctx context.Context, roomID string) error {
	return c.do(ctx, http.MethodPost, "/v1/rooms/"+roomID+"/leave", nil, nil, &statusResponse{})
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
		q.Set("before", before.UTC().Format(time.RFC3339))
	}
	var messages []Message
	err := c.do(ctx, http.MethodGet, "/v1/rooms/"+roomID+"/messages", q, nil, &messages)
	return messages, err
}

// SendMessage posts content to a room as the authenticated user.
func (c *Client) SendMessage(ctx context.Context, roomID, content, typ string) (Message, error) {
	var m Message
	body := map[string]string{"content": content}
	if typ != "" {
		body["type"] = typ
	}
	err := c.do(ctx, http.MethodPost, "/v1/rooms/"+roomID+"/messages", nil, body, &m)
	return m, err
}
