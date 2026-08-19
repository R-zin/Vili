package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// typingThrottle is the minimum interval between typing notifications we emit.
const typingThrottle = 2 * time.Second

// writeTimeout bounds a single websocket write.
const writeTimeout = 5 * time.Second

// readLines reads newline-terminated input on a goroutine, delivering each
// trimmed line on the returned channel. It returns when the input is exhausted
// or ctx is cancelled; the channel is then left open so the chat loop can tell
// "no more input" from "cancelled" via ctx.
func readLines(ctx context.Context, in io.Reader) <-chan string {
	lines := make(chan string)
	go func() {
		defer close(lines)
		reader := bufio.NewReader(in)
		for {
			line, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return
			}
			if s := strings.TrimSpace(line); s != "" {
				select {
				case lines <- s:
				case <-ctx.Done():
					return
				}
			}
			if errors.Is(err, io.EOF) {
				return
			}
		}
	}()
	return lines
}

// readEvents reads events from the websocket on a goroutine until the socket
// closes, pushing them onto push (shared with the caller so tests can inject
// extra events). On read/close error it closes push.
func readEvents(conn *websocket.Conn, push chan wsEvent) {
	go func() {
		defer close(push)
		for {
			e, err := recvEvent(context.Background(), conn)
			if err != nil {
				return
			}
			push <- e
		}
	}()
}

// runRealtimeChat is the live view loop. A single select owns every write to
// out (so nothing interleaves); input and the socket feed it via channels.
// Messages render as they arrive — no Enter required. push carries socket
// events (supplied by readEvents; tests may inject into it too). It returns
// nil on /quit, /leave, or input exhaustion.
func runRealtimeChat(ctx context.Context, out io.Writer, c *Client, roomID string, conn *websocket.Conn, lines <-chan string, push <-chan wsEvent) error {
	var lastTyping time.Time

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "\nbye")
			return nil

		case line, ok := <-lines:
			if !ok {
				// Input closed (EOF/Ctrl-D): leave quietly.
				return nil
			}
			// A completed line means the user was typing; let the room know
			// (throttled), then dispatch the command or message.
			noteTyping(ctx, conn, &lastTyping)
			switch line {
			case "/quit":
				fmt.Fprintln(out, "bye")
				return nil
			case "/leave":
				if err := c.Leave(ctx, roomID); err != nil {
					return err
				}
				fmt.Fprintln(out, "left the room")
				return nil
			default:
				if _, err := c.SendMessage(ctx, roomID, line, ""); err != nil {
					fmt.Fprintf(out, "  · send failed: %s\n", friendlyError(err))
				}
			}

		case e, ok := <-push:
			if !ok {
				return errors.New("realtime connection closed")
			}
			renderEvent(out, e)
		}
	}
}

// renderEvent writes one realtime event in chat form. The sender's own
// message echo is indistinguishable from others' messages, so it renders the
// same — the source of truth is the server.
func renderEvent(out io.Writer, e wsEvent) {
	switch e.Type {
	case eventMessageNew:
		var m Message
		if err := json.Unmarshal(e.Payload, &m); err == nil {
			printMessages(out, []Message{m})
		}
	case eventTyping:
		var p typingPayload
		if err := json.Unmarshal(e.Payload, &p); err == nil && p.Username != "" {
			fmt.Fprintf(out, "  · %s is typing…\n", p.Username)
		}
	case eventPresenceJoin:
		var p presencePayload
		if err := json.Unmarshal(e.Payload, &p); err == nil && p.Username != "" {
			fmt.Fprintf(out, "  · %s joined\n", p.Username)
		}
	case eventPresenceLeave:
		var p presencePayload
		if err := json.Unmarshal(e.Payload, &p); err == nil && p.Username != "" {
			fmt.Fprintf(out, "  · %s left\n", p.Username)
		}
	case eventPresenceState:
		var p presencePayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			if len(p.Online) == 0 {
				fmt.Fprintln(out, "  · you're the only one here")
			} else {
				fmt.Fprintf(out, "  · online: %s\n", strings.Join(p.Online, ", "))
			}
		}
	}
}

// noteTyping sends a typing notification if the throttle window has elapsed.
// It uses its own short timeout so a stalled socket never blocks the loop, and
// the parent ctx — which may be near-done at shutdown — does not cancel it.
func noteTyping(ctx context.Context, conn *websocket.Conn, last *time.Time) {
	if time.Since(*last) < typingThrottle {
		return
	}
	sendTyping(ctx, conn)
	*last = time.Now()
}

// friendlyError reduces an error to a short human message for the chat view.
func friendlyError(err error) string {
	var cerr *Error
	if errors.As(err, &cerr) {
		return cerr.Message
	}
	return err.Error()
}
