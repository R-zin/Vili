package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// defaultServer is used when neither a flag nor a stored session says otherwise.
const defaultServer = "http://localhost:8080"

// Version is the build version printed by "vili version". Release builds
// (GoReleaser) inject it via -ldflags "-X .../internal/cli.version=x.y.z";
// `go build` leaves the "dev" default.
var version = "dev"

// Version returns the build version string.
func Version() string { return version }

// usage is printed for no/unknown subcommand and on --help.
const usage = `vili — terminal client for the Vili chat backend

Usage:
  vili [--server URL] <command> [args]

Account:
  register <username> [password]     create an account (password may be piped on stdin)
  login <username> [password]        sign in and save the session
  logout                             clear the saved session
  whoami                             show the signed-in account
  passwd <current> [new]             change password (signs out other sessions)
  sessions [revoke <id> | revoke-all]  list or sign out devices
  delete-account [password] --yes    permanently delete your account

Rooms:
  rooms [--mine] [--search NAME]     list rooms (with unread counts for yours)
  unread                             only rooms with unread messages
  create <name> [description] [--private]
                                     create a room (you become owner)
  join <room-id>                     join a public room
  leave <room-id>                    leave a room
  read <room-id>                     mark a room as read
  dm <username>                      open a direct message
  edit-room <room-id> [--name N] [--desc D] [--private true|false]
  delete-room <room-id> --yes        delete a room (owner)

Membership (owner/admin):
  members <room-id>                  list members
  kick <room-id> <username>          remove a member
  promote|demote <room-id> <username>  make/unmake an admin (owner)
  invite <room-id> [--uses N] [--ttl-hours H]   create an invite code
  invites <room-id>                  list invites
  revoke-invite <room-id> <invite-id>
  redeem <invite-code>               join a room with an invite code

Messages:
  history <room-id> [--limit N] [--before RFC3339] [--ids]
                                     print room history, oldest first
  send <room-id> <message...> [--type text|diff|code|log|commit] [--reply ID]
  send <room-id> --file PATH|- [--type diff]
                                     send a message from a file or stdin
  edit <room-id> <message-id> <new text...>
  delete <room-id> <message-id>
  search <query...> [--room ID] [--limit N]
  chat <room-id>                     live view: realtime messages, presence, typing

Look & feel:
  theme                              interactive theme picker (arrow keys, live preview)
  theme list | set <name> | preview [name]
  config                             show settings and the keys you can change
  config set <key> <value>           e.g. config set color.mention "#ff79c6"
  config unset <key> | reset | path

  version                            print the build version

Global flags:
  --server URL   backend base URL (default http://localhost:8080; persists in session)
  --token  JWT   override the saved session token
  --theme NAME   use a theme for this command only (or VILI_THEME)
  --color MODE   auto | always | never (or VILI_COLOR; NO_COLOR is honored)
  --no-anim      disable spinners and animations (or VILI_NO_ANIM=1)
`

// Run dispatches a subcommand. out receives normal output; in supplies stdin
// for interactive commands (chat). Errors are returned, not printed, so main
// stays a thin wrapper.
func Run(ctx context.Context, out io.Writer, in io.Reader, args []string) error {
	// Global flags that may precede the subcommand.
	fs := flag.NewFlagSet("vili", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", "", "backend base URL")
	tokenOverride := fs.String("token", "", "override session token")
	themeFlag := fs.String("theme", "", "theme for this invocation")
	colorFlag := fs.String("color", "", "auto|always|never")
	noAnim := fs.Bool("no-anim", false, "disable animations")
	rest, err := parseLeadingFlags(fs, args)

	// Presentation: saved prefs + flags + environment. A broken config file
	// must never brick the CLI (it would block `vili config reset`), so fall
	// back to defaults with a warning.
	prefs, perr := LoadPrefs()
	if perr != nil {
		fmt.Fprintf(stderr, "vili: warning: %v (using defaults; fix with: vili config reset)\n", perr)
	}
	ui, uerr := newUI(out, prefs, uiOverrides{Theme: *themeFlag, Color: *colorFlag, NoAnim: *noAnim}, os.Getenv)
	if uerr != nil {
		return uerr
	}
	for _, w := range ui.Warnings {
		fmt.Fprintf(stderr, "vili: warning: %s (fix with: vili config)\n", w)
	}

	if err != nil {
		fmt.Fprint(out, renderUsage(ui.Style))
		return err
	}
	if len(rest) == 0 {
		fmt.Fprint(out, renderUsage(ui.Style))
		return errors.New("no command given")
	}

	cmd, cmdArgs := rest[0], rest[1:]

	// Commands that need no session, client, or network.
	switch cmd {
	case "version":
		fmt.Fprintf(out, "vili %s\n", version)
		return nil
	case "theme", "themes":
		return cmdTheme(out, in, ui, cmdArgs)
	case "config":
		return cmdConfig(out, ui, cmdArgs)
	case "help", "--help", "-h":
		fmt.Fprint(out, renderUsage(ui.Style))
		return nil
	}

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
	if sess != nil {
		client.Username = sess.User.Username
	}
	client.UI = ui
	label := spinLabels[cmd]
	if label == "" {
		label = "talking to " + baseURL
	}
	client.Busy = func() func() { return ui.Spin(label) }
	// Wire silent renewal: when the access token nears expiry, the client
	// rotates it with the stored refresh token and persists the new pair.
	if sess != nil && *tokenOverride == "" {
		client.RefreshToken = sess.RefreshToken
		client.TokenExpiry = sess.TokenExpiry
		client.OnTokenRefresh = func(access, refresh string, expiry time.Time) {
			sess.Token, sess.RefreshToken, sess.TokenExpiry = access, refresh, expiry
			_ = sess.Save() // best-effort; a lost save just forces re-login next time
		}
	}

	switch cmd {
	case "register":
		return cmdRegister(ctx, out, in, client, cmdArgs)
	case "login":
		return cmdLogin(ctx, out, in, client, baseURL, cmdArgs)
	case "logout":
		return cmdLogout(ctx, out, client)
	case "whoami":
		return cmdWhoami(ctx, out, client)
	case "passwd":
		return cmdPasswd(ctx, out, in, client, cmdArgs)
	case "sessions":
		return cmdSessions(ctx, out, client, cmdArgs)
	case "delete-account":
		return cmdDeleteAccount(ctx, out, in, client, cmdArgs)
	case "rooms":
		return cmdRooms(ctx, out, client, cmdArgs)
	case "unread":
		return cmdUnread(ctx, out, client)
	case "create":
		return cmdCreateRoom(ctx, out, client, cmdArgs)
	case "join":
		return cmdJoin(ctx, out, client, cmdArgs)
	case "leave":
		return cmdLeave(ctx, out, client, cmdArgs)
	case "read":
		return cmdRead(ctx, out, client, cmdArgs)
	case "dm":
		return cmdDM(ctx, out, client, cmdArgs)
	case "edit-room":
		return cmdEditRoom(ctx, out, client, cmdArgs)
	case "delete-room":
		return cmdDeleteRoom(ctx, out, client, cmdArgs)
	case "members":
		return cmdMembers(ctx, out, client, cmdArgs)
	case "kick":
		return cmdKick(ctx, out, client, cmdArgs)
	case "promote":
		return cmdRole(ctx, out, client, "admin", cmdArgs)
	case "demote":
		return cmdRole(ctx, out, client, "member", cmdArgs)
	case "invite":
		return cmdInvite(ctx, out, client, cmdArgs)
	case "invites":
		return cmdInvites(ctx, out, client, cmdArgs)
	case "revoke-invite":
		return cmdRevokeInvite(ctx, out, client, cmdArgs)
	case "redeem":
		return cmdRedeem(ctx, out, client, cmdArgs)
	case "history":
		return cmdHistory(ctx, out, client, cmdArgs)
	case "send":
		return cmdSend(ctx, out, in, client, cmdArgs)
	case "edit":
		return cmdEditMessage(ctx, out, client, cmdArgs)
	case "delete":
		return cmdDeleteMessage(ctx, out, client, cmdArgs)
	case "search":
		return cmdSearch(ctx, out, client, cmdArgs)
	case "chat":
		return cmdChat(ctx, out, in, client, cmdArgs)
	default:
		fmt.Fprint(out, renderUsage(ui.Style))
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// spinLabels are the spinner captions shown while a command waits on the
// server.
var spinLabels = map[string]string{
	"register": "creating your account", "login": "signing in", "rooms": "loading rooms",
	"unread": "checking unread", "history": "loading history", "send": "sending",
	"search": "searching", "chat": "joining", "members": "loading members",
	"invite": "minting invite", "redeem": "redeeming invite", "dm": "opening conversation",
	"passwd": "updating password", "sessions": "loading sessions", "whoami": "loading profile",
}

// renderUsage colors the help text: section headings and command names.
func renderUsage(st *Style) string {
	if !st.Enabled() {
		return usage
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(usage, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(trimmed, "vili —"):
			b.WriteString(st.Gradient(trimmed, 0, gradientStops(st.Theme())...) + "\n")
		case trimmed != "" && !strings.HasPrefix(trimmed, " ") && strings.HasSuffix(trimmed, ":"):
			b.WriteString(st.Accent(trimmed) + "\n")
		case strings.HasPrefix(trimmed, "  ") && !strings.HasPrefix(trimmed, "   "):
			body := trimmed[2:]
			word, tail, _ := strings.Cut(body, " ")
			b.WriteString("  " + st.paint(word, st.Theme().Accent) + " " + st.Muted(tail) + "\n")
		default:
			b.WriteString(line)
		}
	}
	return b.String()
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

func cmdRegister(ctx context.Context, out io.Writer, in io.Reader, c *Client, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: vili register <username> [password]")
	}
	pw, err := secretArg(in, args, 1, "password")
	if err != nil {
		return err
	}
	u, err := c.Register(ctx, args[0], pw)
	if err != nil {
		return err
	}
	c.UI.ok(out, "registered %s (id %s) — now run: vili login %s <password>", u.Username, u.ID, u.Username)
	return nil
}

func cmdLogin(ctx context.Context, out io.Writer, in io.Reader, c *Client, baseURL string, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: vili login <username> [password]")
	}
	pw, err := secretArg(in, args, 1, "password")
	if err != nil {
		return err
	}
	resp, err := c.Login(ctx, args[0], pw)
	if err != nil {
		return err
	}
	sess := &Session{
		BaseURL:      baseURL,
		Token:        resp.Token,
		RefreshToken: resp.RefreshToken,
		TokenExpiry:  resp.ExpiresAt,
		User:         resp.User,
	}
	if err := sess.Save(); err != nil {
		return err
	}
	name := sanitizeTerminal(resp.User.Username)
	if st := c.UI.style(); st.Enabled() {
		c.UI.Typewrite(out, st.Success("✓")+" logged in as "+st.User(name)+" "+st.Muted("— session saved")+" "+st.Mention("✦"))
	} else {
		fmt.Fprintf(out, "logged in as %s — session saved\n", name)
	}
	return nil
}

func cmdLogout(ctx context.Context, out io.Writer, c *Client) error {
	// Best-effort server-side revocation so the refresh token can't be reused
	// after the session file is gone; a network failure must not block logout.
	_ = c.Logout(ctx)
	if err := ClearSession(); err != nil {
		return err
	}
	c.UI.ok(out, "logged out — session cleared")
	return nil
}

func cmdJoin(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "join <room-id>"); err != nil {
		return err
	}
	if err := c.Join(ctx, args[0]); err != nil {
		return err
	}
	c.UI.ok(out, "joined")
	return nil
}

func cmdLeave(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "leave <room-id>"); err != nil {
		return err
	}
	if err := c.Leave(ctx, args[0]); err != nil {
		return err
	}
	c.UI.ok(out, "left")
	return nil
}

func cmdHistory(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := newFlagSet("history")
	limit := fs.Int("limit", 0, "max messages (default server limit)")
	before := fs.String("before", "", "only messages older than this RFC3339 time")
	ids := fs.Bool("ids", false, "show message ids (for reply/edit/delete)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if err := requireArgs(pos, 1, "history <room-id> [--limit N] [--before RFC3339] [--ids]"); err != nil {
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
	o := c.UI.opts(c.Username)
	o.IDs = *ids
	printMessagesOpts(out, messages, o)
	return nil
}

// printOpts tunes message rendering.
type printOpts struct {
	// IDs prefixes each message with its full id (needed to reply/edit/delete).
	IDs bool
	// Me is the signed-in username; messages that @mention it are flagged.
	Me string
	// Style colors the output; nil renders plain text.
	Style *Style
	// TimeLayout formats timestamps ("" hides them; zero value means 15:04:05
	// when Style is nil, so plain callers keep the historical format).
	TimeLayout string
}

// printMessages writes messages oldest-first in a chat-like form,
// sanitizing ANSI escape codes and control characters to prevent terminal injection.
func printMessages(out io.Writer, messages []Message) {
	printMessagesOpts(out, messages, printOpts{})
}

func printMessagesOpts(out io.Writer, messages []Message, o printOpts) {
	for _, m := range messages {
		fmt.Fprintln(out, formatMessage(m, o))
	}
}

// mentionsMe reports whether m @mentions the signed-in user.
func mentionsMe(m Message, me string) bool {
	if me == "" || m.Deleted {
		return false
	}
	for _, name := range m.Mentions {
		if name == me {
			return true
		}
	}
	return false
}

// formatMessage renders one message. Untrusted fields are sanitized BEFORE any
// styling is added, so a message can never inject escape codes of its own.
// Deleted messages show a tombstone, edited ones are marked, replies show the
// short id of their target, and messages that @mention the signed-in user are
// flagged with a leading "»". Multi-line diff/code/log/commit payloads are
// shown in an indented gutter.
func formatMessage(m Message, o printOpts) string {
	st := o.Style
	layout := o.TimeLayout
	if layout == "" && st == nil {
		layout = "15:04:05"
	}
	var b strings.Builder
	if mentionsMe(m, o.Me) {
		b.WriteString(st.Mention("»") + " ")
	}
	if o.IDs {
		b.WriteString(st.Muted(m.ID) + "  ")
	}
	if layout != "" {
		b.WriteString(st.Time(m.CreatedAt.Local().Format(layout)) + "  ")
	}
	b.WriteString(st.User(sanitizeTerminal(m.Username)))
	if m.ReplyTo != "" {
		b.WriteString(st.Muted(" (↪ " + shortID(m.ReplyTo) + ")"))
	}
	b.WriteString(": ")
	if m.Deleted {
		b.WriteString(st.Muted(st.Italic("[message deleted]")))
		return b.String()
	}
	b.WriteString(renderContent(m.Type, sanitizeTerminal(m.Content), m.Mentions, o.Me, st))
	if m.EditedAt != nil {
		b.WriteString(st.Muted(" (edited)"))
	}
	return b.String()
}

// renderContent styles already-sanitized content by message type.
func renderContent(typ, content string, mentions []string, me string, st *Style) string {
	lineStyle := func(line string) string {
		switch typ {
		case "diff":
			switch {
			case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
				return st.Bold(line)
			case strings.HasPrefix(line, "+"):
				return st.Add(line)
			case strings.HasPrefix(line, "-"):
				return st.Del(line)
			case strings.HasPrefix(line, "@@"):
				return st.Accent(line)
			}
			return line
		case "code":
			return st.Code(line)
		case "log":
			return st.Muted(line)
		}
		return line
	}
	gutterTypes := typ == "diff" || typ == "code" || typ == "log" || typ == "commit"
	if gutterTypes && strings.Contains(content, "\n") {
		var b strings.Builder
		for i, line := range strings.Split(content, "\n") {
			b.WriteString("\n  " + st.Muted("│") + " ")
			if typ == "commit" && i == 0 {
				b.WriteString(st.Accent(line))
				continue
			}
			b.WriteString(lineStyle(line))
		}
		return b.String()
	}
	if typ == "commit" {
		return st.Code(content)
	}
	if gutterTypes {
		return lineStyle(content)
	}
	return highlightMentions(content, mentions, me, st)
}

// highlightMentions colors "@name" for each resolved mention: yours stand out,
// others are accented. Plain styles return content unchanged.
func highlightMentions(content string, mentions []string, me string, st *Style) string {
	if !st.Enabled() || len(mentions) == 0 {
		return content
	}
	pairs := make([]string, 0, 2*len(mentions))
	for _, name := range mentions {
		tag := "@" + name
		if name == me {
			pairs = append(pairs, tag, st.Mention(tag))
		} else {
			pairs = append(pairs, tag, st.Accent(tag))
		}
	}
	return strings.NewReplacer(pairs...).Replace(content)
}

// shortID abbreviates a UUID for display.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sanitizeTerminal strips ANSI escape sequences and non-printable control characters
// (except newline and tab) from untrusted user input before terminal rendering.
func sanitizeTerminal(s string) string {
	const (
		stateNormal = iota
		stateEscape
		stateCSI
		stateOSC
		stateOSCEscape
		stateCharset
	)

	var b strings.Builder
	b.Grow(len(s))
	state := stateNormal

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case stateNormal:
			if c == 0x1b { // ESC
				state = stateEscape
			} else if c == '\n' || c == '\t' {
				b.WriteByte(c)
			} else if c < 32 || c == 127 {
				// Strip raw ASCII control characters
				continue
			} else {
				b.WriteByte(c)
			}
		case stateEscape:
			switch c {
			case '[':
				state = stateCSI
			case ']':
				state = stateOSC
			case '(', ')', '*', '+':
				state = stateCharset
			default:
				// Single byte following ESC ends sequence (e.g., ESC c, ESC M)
				state = stateNormal
			}
		case stateCSI:
			// Final CSI byte is in range 0x40-0x7E (@ through ~)
			if c >= 0x40 && c <= 0x7E {
				state = stateNormal
			}
		case stateOSC:
			if c == 0x07 { // BEL
				state = stateNormal
			} else if c == 0x1b {
				state = stateOSCEscape
			}
		case stateOSCEscape:
			if c == '\\' { // ST (String Terminator: ESC \)
				state = stateNormal
			} else if c == '[' {
				state = stateCSI
			} else if c == ']' {
				state = stateOSC
			} else {
				state = stateNormal
			}
		case stateCharset:
			state = stateNormal
		}
	}
	return b.String()
}
