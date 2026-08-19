package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
)

// wsURL derives the realtime websocket URL for a room from the client's base
// HTTP URL: http→ws, https→wss, path /v1/rooms/{id}/ws.
func (c *Client) wsURL(roomID string) (string, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return "", fmt.Errorf("parse server URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already a websocket scheme
	default:
		return "", fmt.Errorf("unsupported server scheme %q (want http/https)", u.Scheme)
	}
	u.Path = "/v1/rooms/" + roomID + "/ws"
	u.RawQuery = ""
	return u.String(), nil
}

// watch opens the realtime websocket for a room, authenticating with the
// session bearer token on the upgrade. The caller owns the returned connection
// and must close it. A non-101 response is decoded from the error envelope.
func (c *Client) watch(ctx context.Context, roomID string) (*websocket.Conn, error) {
	if c.Token == "" {
		return nil, ErrNoSession
	}
	u, err := c.wsURL(roomID)
	if err != nil {
		return nil, err
	}
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.Token}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, errors.New("room not found or you are not a member")
		}
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, errors.New("authentication failed — run: vili login")
		}
		return nil, fmt.Errorf("connect realtime: %w", err)
	}
	return conn, nil
}

// sendTyping notifies the room that the user is composing. Best-effort: a
// transient relay failure is ignored.
func sendTyping(ctx context.Context, conn *websocket.Conn) {
	payload := []byte(`{"type":"typing"}`)
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	_ = conn.Write(wctx, websocket.MessageText, payload)
}

// recvEvent reads and decodes the next event from the websocket.
func recvEvent(ctx context.Context, conn *websocket.Conn) (wsEvent, error) {
	var e wsEvent
	_, data, err := conn.Read(ctx)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return e, fmt.Errorf("decode event: %w", err)
	}
	return e, nil
}
