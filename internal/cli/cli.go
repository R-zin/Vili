package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// defaultServer is used when neither a flag nor a stored session says otherwise.
const defaultServer = "http://localhost:8080"

// usage is printed for no/unknown subcommand and on --help.
const usage = `vili — terminal client for the Vili chat backend

Usage:
  vili [--server URL] <command> [args]

Commands:
  register <username> <password>     create an account
  login <username> <password>        sign in and save the session
  logout                             clear the saved session
  rooms                              list all rooms
  create <name> [description]        create a room (you become owner)
  join <room-id>                     join a room
  leave <room-id>                    leave a room
  history <room-id> [--limit N] [--before RFC3339]
                                     print room history, oldest first
  send <room-id> <message...> [--type text|diff|code|log|commit]
                                     post a message
  chat <room-id>                     interactive view: watch + type messages

Global flags:
  --server URL   backend base URL (default http://localhost:8080; persists in session)
  --token  JWT   override the saved session token
`

// Run dispatches a subcommand. out receives normal output; in supplies stdin
// for interactive commands (chat). Errors are returned, not printed, so main
// stays a thin wrapper.
func Run(ctx context.Context, out io.Writer, in io.Reader, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return errors.New("no command given")
	}

	// Global flags that may precede the subcommand.
	fs := flag.NewFlagSet("vili", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "backend base URL")
	tokenOverride := fs.String("token", "", "override session token")
	rest, err := parseLeadingFlags(fs, args)
	if err != nil {
		fmt.Fprint(out, usage)
		return err
	}
	if len(rest) == 0 {
		fmt.Fprint(out, usage)
		return errors.New("no command given")
	}

	cmd, cmdArgs := rest[0], rest[1:]

	// Resolve session + base URL. Flag wins, then stored session, then default.
	sess, _ := LoadSession() // ignore ErrNoSession; treated as nil
	baseURL := defaultServer
	if sess != nil && sess.BaseURL != "" {
		baseURL = sess.BaseURL
	}
	if *server != "" {
		baseURL = *server
	}
	client := NewClient(baseURL, sessionToken(sess, *tokenOverride))

	switch cmd {
	case "register":
		return cmdRegister(ctx, out, client, cmdArgs)
	case "login":
		return cmdLogin(ctx, out, client, baseURL, cmdArgs)
	case "logout":
		return cmdLogout(out)
	case "rooms":
		return cmdRooms(ctx, out, client)
	case "create":
		return cmdCreateRoom(ctx, out, client, cmdArgs)
	case "join":
		return cmdJoin(ctx, out, client, cmdArgs)
	case "leave":
		return cmdLeave(ctx, out, client, cmdArgs)
	case "history":
		return cmdHistory(ctx, out, client, cmdArgs)
	case "send":
		return cmdSend(ctx, out, client, cmdArgs)
	case "chat":
		return cmdChat(ctx, out, in, client, cmdArgs)
	case "help", "--help", "-h":
		fmt.Fprint(out, usage)
		return nil
	default:
		fmt.Fprint(out, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// sessionToken prefers an explicit override, else the saved session token.
func sessionToken(sess *Session, override string) string {
	if override != "" {
		return override
	}
	if sess != nil {
		return sess.Token
	}
	return ""
}

// parseLeadingFlags parses global flags that appear before the subcommand and
// returns the remaining args (subcommand and its own args). Subcommand args
// are parsed separately by each command.
func parseLeadingFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	// flag stops at the first non-flag arg, which is exactly the subcommand.
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return fs.Args(), nil
}

// requireArgs enforces an exact positional-arg count, returning a usage-style
// error that includes the command's form.
func requireArgs(cmdArgs []string, n int, form string) error {
	if len(cmdArgs) != n {
		return fmt.Errorf("usage: vili %s", form)
	}
	return nil
}

func cmdRegister(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 2, "register <username> <password>"); err != nil {
		return err
	}
	u, err := c.Register(ctx, args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "registered %s (id %s) — now run: vili login %s <password>\n", u.Username, u.ID, u.Username)
	return nil
}

func cmdLogin(ctx context.Context, out io.Writer, c *Client, baseURL string, args []string) error {
	if err := requireArgs(args, 2, "login <username> <password>"); err != nil {
		return err
	}
	token, u, err := c.Login(ctx, args[0], args[1])
	if err != nil {
		return err
	}
	sess := &Session{BaseURL: baseURL, Token: token, User: u}
	if err := sess.Save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "logged in as %s — session saved\n", u.Username)
	return nil
}

func cmdLogout(out io.Writer) error {
	if err := ClearSession(); err != nil {
		return err
	}
	fmt.Fprintln(out, "logged out — session cleared")
	return nil
}

func cmdRooms(ctx context.Context, out io.Writer, c *Client) error {
	rooms, err := c.ListRooms(ctx)
	if err != nil {
		return err
	}
	if len(rooms) == 0 {
		fmt.Fprintln(out, "no rooms yet — create one with: vili create <name>")
		return nil
	}
	for _, r := range rooms {
		desc := ""
		if r.Description != "" {
			desc = "  — " + r.Description
		}
		fmt.Fprintf(out, "%s  %s%s\n", r.ID, r.Name, desc)
	}
	return nil
}

func cmdCreateRoom(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: vili create <name> [description]")
	}
	desc := ""
	if len(args) == 2 {
		desc = args[1]
	}
	r, err := c.CreateRoom(ctx, args[0], desc)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "created %s (id %s)\n", r.Name, r.ID)
	return nil
}

func cmdJoin(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "join <room-id>"); err != nil {
		return err
	}
	if err := c.Join(ctx, args[0]); err != nil {
		return err
	}
	fmt.Fprintln(out, "joined")
	return nil
}

func cmdLeave(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "leave <room-id>"); err != nil {
		return err
	}
	if err := c.Leave(ctx, args[0]); err != nil {
		return err
	}
	fmt.Fprintln(out, "left")
	return nil
}

func cmdHistory(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 0, "max messages (default server limit)")
	before := fs.String("before", "", "only messages older than this RFC3339 time")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if err := requireArgs(pos, 1, "history <room-id> [--limit N] [--before RFC3339]"); err != nil {
		return err
	}
	var beforePtr *time.Time
	if *before != "" {
		t, err := time.Parse(time.RFC3339, *before)
		if err != nil {
			return fmt.Errorf("--before must be RFC3339: %w", err)
		}
		beforePtr = &t
	}
	messages, err := c.History(ctx, pos[0], *limit, beforePtr)
	if err != nil {
		return err
	}
	printMessages(out, messages)
	return nil
}

func cmdSend(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	typ := fs.String("type", "", "message type: text|diff|code|log|commit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("usage: vili send <room-id> <message...> [--type ...]")
	}
	m, err := c.SendMessage(ctx, pos[0], strings.Join(pos[1:], " "), *typ)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sent (id %s)\n", m.ID)
	return nil
}

// printMessages writes messages oldest-first in a chat-like form.
func printMessages(out io.Writer, messages []Message) {
	for _, m := range messages {
		fmt.Fprintf(out, "%s  %s: %s\n", m.CreatedAt.Local().Format("15:04:05"), m.Username, m.Content)
	}
}
