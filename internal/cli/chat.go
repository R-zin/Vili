package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// pollInterval is how often the (fallback) polling chat view refetches history.
const pollInterval = 2 * time.Second

// recentCount is how many messages are shown when entering chat.
const recentCount = 20

// cmdChat runs the interactive view. It shows recent history, then prefers the
// realtime websocket (messages render as they arrive); if the socket can't be
// established it falls back to polling. /quit or Ctrl-C exits; /leave leaves.
func cmdChat(parent context.Context, out io.Writer, in io.Reader, c *Client, args []string) error {
	if err := requireArgs(args, 1, "chat <room-id>"); err != nil {
		return err
	}
	roomID := args[0]
	if c.Token == "" {
		return ErrNoSession
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Opening state: show recent history so the room has context on entry.
	messages, err := c.History(ctx, roomID, recentCount, nil)
	if err != nil {
		return err
	}
	// From here on, output interleaves with the user's typing: no spinners.
	c.Busy = nil
	if c.UI.style().Enabled() {
		c.UI.Banner(out, "◆ vili · "+roomTitle(ctx, c, roomID))
	}
	printMessagesOpts(out, messages, c.UI.opts(c.Username))
	cursor := cursorFrom(messages)

	// Everything shown is now read; mark it so unread counters stay accurate,
	// and again on exit to cover what arrived while the view was open. Best
	// effort: a failure here must never break chatting.
	_ = c.MarkRead(ctx, roomID)
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer rcancel()
		_ = c.MarkRead(rctx, roomID)
	}()

	// Try realtime first: open the websocket and, on success, run the live loop.
	conn, err := c.watch(ctx, roomID)
	if err == nil {
		fmt.Fprintln(out, c.UI.style().Muted("— live chat: type a message and press enter; /quit to exit, /leave to leave —"))
		defer conn.Close(websocket.StatusNormalClosure, "leaving")
		push := make(chan wsEvent)
		readEvents(conn, push)
		return runRealtimeChat(ctx, out, c, roomID, conn, readLines(ctx, in), push)
	}

	// Fall back to polling when realtime is unavailable.
	fmt.Fprintf(out, "— chat (polling, realtime unavailable: %s): /quit to exit, /leave to leave —\n", friendlyError(err))
	return runPollingChat(ctx, out, in, c, roomID, cursor)
}

// roomTitle returns a display name for the room header, falling back to a
// short id if the listing cannot be fetched.
func roomTitle(ctx context.Context, c *Client, roomID string) string {
	rooms, err := c.ListRoomsFiltered(ctx, "", true)
	if err == nil {
		for _, r := range rooms {
			if r.ID == roomID {
				return roomLabel(r)
			}
		}
	}
	return shortID(roomID)
}

// runPollingChat is the original poll-based view, kept as the fallback when no
// websocket can be opened. It renders history, then loops reading a line and
// polling for what arrived after each.
func runPollingChat(ctx context.Context, out io.Writer, in io.Reader, c *Client, roomID string, cursor *time.Time) error {
	reader := bufio.NewReader(in)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "\nbye")
			return nil
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read input: %w", err)
		}
		if errors.Is(err, io.EOF) && line == "" {
			fmt.Fprintln(out, "\nbye")
			return nil
		}

		cursor, err = handleChatLine(ctx, out, c, roomID, strings.TrimSpace(line), cursor)
		if err != nil {
			if errors.Is(err, errQuit) {
				fmt.Fprintln(out, "bye")
				return nil
			}
			if errors.Is(err, errLeft) {
				return nil
			}
			return err
		}
	}
}

// Sender/reporter signals for the polling loop, distinguished from real errors
// so the loop can tell an intentional exit from a failure.
var (
	errQuit = errors.New("quit requested")
	errLeft = errors.New("left room")
)

// cursorFrom returns the latest created_at across messages, or nil when empty.
func cursorFrom(messages []Message) *time.Time {
	for i := len(messages) - 1; i >= 0; i-- {
		if !messages[i].CreatedAt.IsZero() {
			t := messages[i].CreatedAt
			return &t
		}
	}
	return nil
}

// handleChatLine processes one input line for the polling view: slash-commands
// act locally, blank lines just refresh, anything else is sent. It then polls
// for and prints new messages and returns the advanced cursor. A nil cursor is
// preserved when the room has no messages yet.
func handleChatLine(ctx context.Context, out io.Writer, c *Client, roomID, line string, cursor *time.Time) (*time.Time, error) {
	switch {
	case line == "/quit":
		return cursor, errQuit
	case line == "/leave":
		if err := c.Leave(ctx, roomID); err != nil {
			return cursor, err
		}
		fmt.Fprintln(out, "left the room")
		return cursor, errLeft
	case line == "":
		// no-op: fall through to the poll below
	default:
		if _, err := c.SendMessage(ctx, roomID, line, ""); err != nil {
			return cursor, fmt.Errorf("send: %w", err)
		}
	}

	select {
	case <-ctx.Done():
		return cursor, errQuit
	case <-time.After(pollInterval):
	}

	fresh, err := pollAfter(ctx, c, roomID, cursor)
	if err != nil {
		return cursor, err
	}
	printMessagesOpts(out, fresh, c.UI.opts(c.Username))
	if len(fresh) > 0 {
		return cursorFrom(fresh), nil
	}
	return cursor, nil
}

// pollAfter fetches newer-than-cursor messages by walking the before window
// back far enough to catch arrivals, then filtering to strictly-after cursor.
// The backend history is oldest-first and cursor-bounded by `before`, so we
// request a page and drop anything at or before the cursor client-side.
func pollAfter(ctx context.Context, c *Client, roomID string, cursor *time.Time) ([]Message, error) {
	messages, err := c.History(ctx, roomID, recentCount, nil)
	if err != nil {
		return nil, err
	}
	if cursor == nil {
		return messages, nil
	}
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.CreatedAt.After(*cursor) {
			out = append(out, m)
		}
	}
	return out, nil
}
