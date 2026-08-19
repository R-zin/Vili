package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/event"
)

// sendBuffer bounds how many events queue for a client before it is dropped
// as a slow consumer. writeTimeout is the per-frame write deadline; the
// hijacked websocket connection is not subject to the server's WriteTimeout.
const (
	sendBuffer   = 32
	writeTimeout = 10 * time.Second
)

// Client is one live websocket connection: a member of a single room. The
// write pump is the only writer to the connection; everyone else (the hub)
// enqueues into send. The read pump relays ephemeral client→server events
// (typing) to the hub.
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	roomID   uuid.UUID
	userID   uuid.UUID
	username string

	// send is the bounded outbound queue; the hub enqueues here.
	send chan event.Event

	// done is closed when the client should tear down both pumps.
	done chan struct{}
	// closeCancel cancels the per-connection context, unblocking the read.
	closeCancel context.CancelFunc
}

// newClient builds a Client for an accepted connection.
func newClient(hub *Hub, conn *websocket.Conn, roomID, userID uuid.UUID, username string) *Client {
	return &Client{
		hub:      hub,
		conn:     conn,
		roomID:   roomID,
		userID:   userID,
		username: username,
		send:     make(chan event.Event, sendBuffer),
		done:     make(chan struct{}),
	}
}

// run owns the connection: it registers with the hub, runs the write pump in
// the background, and runs the read pump in the foreground until the peer
// disconnects or the hub closes the client. It returns when both are done.
func (c *Client) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.closeCancel = cancel
	defer cancel()

	c.hub.Join(c.roomID, c)
	// Send the freshly-joined client the current presence state, then tell the
	// room someone arrived.
	c.sendNow(event.NewPresenceState(c.roomID, c.hub.Presence(c.roomID)))
	c.hub.Broadcast(c.roomID, event.NewPresenceJoin(c.roomID, c.username))
	defer func() {
		c.hub.Leave(c.roomID, c)
		c.hub.Broadcast(c.roomID, event.NewPresenceLeave(c.roomID, c.username))
	}()

	go c.writePump(ctx)
	c.readPump(ctx)

	// Read ended: tear down the write pump and close the peer connection.
	c.close(websocket.StatusNormalClosure, "closing")
}

// readPump consumes client→server frames. Only typing events are honored;
// anything else is ignored. It returns on read error or cancellation.
func (c *Client) readPump(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var e event.Event
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		if e.Type == event.Typing {
			c.hub.Broadcast(c.roomID, event.NewTyping(c.roomID, c.username))
		}
	}
}

// writePump is the sole writer to the connection: it drains the send queue and
// services the coder/websocket ping/pong keepalive until done.
func (c *Client) writePump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-c.send:
			if err := c.write(ctx, e); err != nil {
				return
			}
		}
	}
}

// write encodes and sends one event with a per-frame deadline.
func (c *Client) write(ctx context.Context, e event.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, data)
}

// sendNow enqueues e, dropping the client as a slow consumer if its buffer is
// full. It never blocks the caller (the hub broadcasts to many clients).
func (c *Client) sendNow(e event.Event) {
	select {
	case c.send <- e:
	default:
		// Slow consumer: drop it so one stalled client can't stall the room.
		slog.Warn("dropping slow websocket client", "user", c.username, "room", c.roomID)
		c.close(websocket.StatusGoingAway, "slow consumer")
	}
}

// close closes done and the peer connection, unblocking both pumps. It is
// idempotent.
func (c *Client) close(status websocket.StatusCode, reason string) {
	select {
	case <-c.done:
		// Already closed.
	default:
		close(c.done)
	}
	if c.closeCancel != nil {
		c.closeCancel()
	}
	// conn is nil only in tests that exercise the registry without a network.
	if c.conn != nil {
		_ = c.conn.Close(status, reason)
	}
}

// isDone reports whether close has been called.
func (c *Client) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}
