package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// maxContentBytes mirrors the server's message size cap so oversize input is
// rejected locally with a clear message rather than after an upload.
const maxContentBytes = 4000

// parseFlags parses flags anywhere in args (Go's flag package stops at the
// first positional), returning the positionals in order. Everything after a
// bare "--" is positional, so message text may start with "-".
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return append(pos, tail...), nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// secretArg returns args[i] when present; otherwise it reads one line from in
// (so a secret can be piped rather than left in shell history). It errors if
// neither yields a value.
func secretArg(in io.Reader, args []string, i int, what string) (string, error) {
	if len(args) > i {
		return args[i], nil
	}
	if in == nil {
		return "", fmt.Errorf("%s required", what)
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read %s: %w", what, err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", fmt.Errorf("%s required (pass it as an argument or pipe it on stdin)", what)
	}
	return line, nil
}

// readContent loads message content from a file path ("-" means stdin).
func readContent(in io.Reader, path string) (string, error) {
	var r io.Reader = in
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxContentBytes+1))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func checkContent(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("message is empty")
	}
	if len(s) > maxContentBytes {
		return fmt.Errorf("message is %d bytes; the limit is %d", len(s), maxContentBytes)
	}
	return nil
}

// ---- rooms ---------------------------------------------------------------

func cmdRooms(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := newFlagSet("rooms")
	mine := fs.Bool("mine", false, "only rooms you belong to")
	search := fs.String("search", "", "filter by name")
	if _, err := parseFlags(fs, args); err != nil {
		return errors.New("usage: vili rooms [--mine] [--search NAME]")
	}
	rooms, err := c.ListRoomsFiltered(ctx, *search, *mine)
	if err != nil {
		return err
	}
	if len(rooms) == 0 {
		fmt.Fprintln(out, "no rooms yet — create one with: vili create <name>")
		return nil
	}
	printRooms(out, rooms, c.UI.style())
	return nil
}

func cmdUnread(ctx context.Context, out io.Writer, c *Client) error {
	rooms, err := c.ListRoomsFiltered(ctx, "", true)
	if err != nil {
		return err
	}
	var unread []Room
	for _, r := range rooms {
		if r.Unread > 0 {
			unread = append(unread, r)
		}
	}
	if len(unread) == 0 {
		fmt.Fprintln(out, "all caught up")
		return nil
	}
	printRooms(out, unread, c.UI.style())
	return nil
}

// roomLabel is the display name of a room: DMs show the other participant.
func roomLabel(r Room) string {
	if r.IsDM && r.Peer != "" {
		return "@" + sanitizeTerminal(r.Peer)
	}
	return sanitizeTerminal(r.Name)
}

func printRooms(out io.Writer, rooms []Room, st *Style) {
	for _, r := range rooms {
		var tags []string
		if r.IsDM {
			tags = append(tags, st.System("dm"))
		} else if r.IsPrivate {
			tags = append(tags, st.System("private"))
		}
		if r.Role != "" && r.Role != "member" {
			tags = append(tags, st.Accent(r.Role))
		}
		if r.Unread > 0 {
			u := st.Warn(fmt.Sprintf("%d unread", r.Unread))
			if r.UnreadMentions > 0 {
				u += ", " + st.Mention(fmt.Sprintf("%d @you", r.UnreadMentions))
			}
			tags = append(tags, u)
		}
		suffix := ""
		if len(tags) > 0 {
			suffix = "  [" + strings.Join(tags, ", ") + "]"
		}
		desc := ""
		if r.Description != "" {
			desc = st.Muted("  — " + sanitizeTerminal(r.Description))
		}
		label := roomLabel(r)
		if r.IsDM {
			label = st.User(label)
		} else {
			label = st.Bold(label)
		}
		fmt.Fprintf(out, "%s  %s%s%s\n", st.Muted(r.ID), label, suffix, desc)
	}
}

func cmdCreateRoom(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := newFlagSet("create")
	private := fs.Bool("private", false, "make the room invite-only")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: vili create <name> [description] [--private]")
	}
	desc := ""
	if len(pos) == 2 {
		desc = pos[1]
	}
	r, err := c.CreateRoomPrivate(ctx, pos[0], desc, *private)
	if err != nil {
		return err
	}
	kind := ""
	if r.IsPrivate {
		kind = " (private)"
	}
	c.UI.ok(out, "created %s%s (id %s)", r.Name, kind, r.ID)
	return nil
}

func cmdEditRoom(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := newFlagSet("edit-room")
	name := fs.String("name", "", "new name")
	desc := fs.String("desc", "", "new description")
	private := fs.String("private", "", "true|false: change visibility (owner only)")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: vili edit-room <room-id> [--name N] [--desc D] [--private true|false]")
	}
	var u RoomUpdate
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			u.Name = name
		case "desc":
			u.Description = desc
		case "private":
			v := *private == "true"
			u.IsPrivate = &v
		}
	})
	if *private != "" && *private != "true" && *private != "false" {
		return errors.New("--private must be true or false")
	}
	if u.Name == nil && u.Description == nil && u.IsPrivate == nil {
		return errors.New("nothing to change: pass --name, --desc or --private")
	}
	r, err := c.UpdateRoom(ctx, pos[0], u)
	if err != nil {
		return err
	}
	c.UI.ok(out, "updated %s", r.Name)
	return nil
}

func cmdDeleteRoom(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := newFlagSet("delete-room")
	yes := fs.Bool("yes", false, "confirm deletion")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: vili delete-room <room-id> --yes")
	}
	if !*yes {
		return errors.New("deleting a room erases all its messages; re-run with --yes to confirm")
	}
	if err := c.DeleteRoom(ctx, pos[0]); err != nil {
		return err
	}
	c.UI.ok(out, "room deleted")
	return nil
}

func cmdRead(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "read <room-id>"); err != nil {
		return err
	}
	if err := c.MarkRead(ctx, args[0]); err != nil {
		return err
	}
	c.UI.ok(out, "marked read")
	return nil
}

func cmdDM(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "dm <username>"); err != nil {
		return err
	}
	r, err := c.OpenDM(ctx, strings.TrimPrefix(args[0], "@"))
	if err != nil {
		return err
	}
	c.UI.ok(out, "direct message with %s (id %s) — vili chat %s", sanitizeTerminal(args[0]), r.ID, r.ID)
	return nil
}

// ---- membership & invites ------------------------------------------------

func cmdMembers(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "members <room-id>"); err != nil {
		return err
	}
	ms, err := c.Members(ctx, args[0])
	if err != nil {
		return err
	}
	for _, m := range ms {
		role := fmt.Sprintf("%-10s", m.Role)
		if m.Role != "member" {
			role = c.UI.style().Accent(role)
		} else {
			role = c.UI.style().Muted(role)
		}
		fmt.Fprintf(out, "%s %s\n", role, c.UI.style().User(sanitizeTerminal(m.Username)))
	}
	return nil
}

// memberID resolves a username in a room to its user id.
func memberID(ctx context.Context, c *Client, roomID, username string) (string, error) {
	username = strings.TrimPrefix(username, "@")
	ms, err := c.Members(ctx, roomID)
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if m.Username == username {
			return m.UserID, nil
		}
	}
	return "", fmt.Errorf("%q is not a member of this room", username)
}

func cmdKick(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 2, "kick <room-id> <username>"); err != nil {
		return err
	}
	id, err := memberID(ctx, c, args[0], args[1])
	if err != nil {
		return err
	}
	if err := c.Kick(ctx, args[0], id); err != nil {
		return err
	}
	c.UI.ok(out, "removed %s", sanitizeTerminal(args[1]))
	return nil
}

func cmdRole(ctx context.Context, out io.Writer, c *Client, role string, args []string) error {
	verb := map[string]string{"admin": "promote", "member": "demote"}[role]
	if err := requireArgs(args, 2, verb+" <room-id> <username>"); err != nil {
		return err
	}
	id, err := memberID(ctx, c, args[0], args[1])
	if err != nil {
		return err
	}
	if err := c.SetRole(ctx, args[0], id, role); err != nil {
		return err
	}
	c.UI.ok(out, "%s is now %s", sanitizeTerminal(args[1]), role)
	return nil
}

func cmdInvite(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := newFlagSet("invite")
	uses := fs.Int("uses", 0, "max redemptions (0 = unlimited)")
	ttl := fs.Int("ttl-hours", 0, "lifetime in hours (default 168)")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New("usage: vili invite <room-id> [--uses N] [--ttl-hours H]")
	}
	code, inv, err := c.CreateInvite(ctx, pos[0], *uses, *ttl)
	if err != nil {
		return err
	}
	// The code itself stays unstyled so it can be copied (and parsed) as is.
	fmt.Fprintf(out, "invite code: %s\n(expires %s — shown once; share with: vili redeem %s)\n",
		code, inv.ExpiresAt.Local().Format(time.RFC822), code)
	return nil
}

func cmdInvites(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "invites <room-id>"); err != nil {
		return err
	}
	invs, err := c.ListInvites(ctx, args[0])
	if err != nil {
		return err
	}
	if len(invs) == 0 {
		fmt.Fprintln(out, "no invites")
		return nil
	}
	for _, i := range invs {
		state := "active"
		switch {
		case i.RevokedAt != nil:
			state = "revoked"
		case time.Now().After(i.ExpiresAt):
			state = "expired"
		case i.MaxUses > 0 && i.Uses >= i.MaxUses:
			state = "used up"
		}
		limit := "∞"
		if i.MaxUses > 0 {
			limit = fmt.Sprint(i.MaxUses)
		}
		st := c.UI.style()
		stateStr := fmt.Sprintf("%-8s", state)
		if state == "active" {
			stateStr = st.Success(stateStr)
		} else {
			stateStr = st.Muted(stateStr)
		}
		fmt.Fprintf(out, "%s  %s uses %d/%s  %s\n", st.Muted(i.ID), stateStr, i.Uses, limit,
			st.Muted("expires "+i.ExpiresAt.Local().Format(time.RFC822)))
	}
	return nil
}

func cmdRevokeInvite(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 2, "revoke-invite <room-id> <invite-id>"); err != nil {
		return err
	}
	if err := c.RevokeInvite(ctx, args[0], args[1]); err != nil {
		return err
	}
	c.UI.ok(out, "invite revoked")
	return nil
}

func cmdRedeem(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 1, "redeem <invite-code>"); err != nil {
		return err
	}
	r, err := c.RedeemInvite(ctx, args[0])
	if err != nil {
		return err
	}
	c.UI.ok(out, "joined %s (id %s)", sanitizeTerminal(r.Name), r.ID)
	return nil
}

// ---- messages ------------------------------------------------------------

func cmdSend(ctx context.Context, out io.Writer, in io.Reader, c *Client, args []string) error {
	fs := newFlagSet("send")
	typ := fs.String("type", "", "message type: text|diff|code|log|commit")
	reply := fs.String("reply", "", "message id to reply to")
	file := fs.String("file", "", "read the message from a file (- for stdin)")
	pos, err := parseFlags(fs, args)
	const form = "usage: vili send <room-id> <message...> [--type T] [--reply ID] | vili send <room-id> --file PATH|-"
	if err != nil || len(pos) < 1 {
		return errors.New(form)
	}
	var content string
	switch {
	case *file != "":
		if len(pos) != 1 {
			return errors.New(form)
		}
		if content, err = readContent(in, *file); err != nil {
			return err
		}
	case len(pos) >= 2:
		content = strings.Join(pos[1:], " ")
	default:
		return errors.New(form)
	}
	if err := checkContent(content); err != nil {
		return err
	}
	m, err := c.SendReply(ctx, pos[0], content, *typ, *reply)
	if err != nil {
		return err
	}
	c.UI.ok(out, "sent (id %s)", m.ID)
	return nil
}

func cmdEditMessage(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if len(args) < 3 {
		return errors.New("usage: vili edit <room-id> <message-id> <new text...>")
	}
	content := strings.Join(args[2:], " ")
	if err := checkContent(content); err != nil {
		return err
	}
	if _, err := c.EditMessage(ctx, args[0], args[1], content); err != nil {
		return err
	}
	c.UI.ok(out, "edited")
	return nil
}

func cmdDeleteMessage(ctx context.Context, out io.Writer, c *Client, args []string) error {
	if err := requireArgs(args, 2, "delete <room-id> <message-id>"); err != nil {
		return err
	}
	if err := c.DeleteMessage(ctx, args[0], args[1]); err != nil {
		return err
	}
	c.UI.ok(out, "deleted")
	return nil
}

func cmdSearch(ctx context.Context, out io.Writer, c *Client, args []string) error {
	fs := newFlagSet("search")
	room := fs.String("room", "", "restrict to one room id")
	limit := fs.Int("limit", 0, "max results")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) < 1 {
		return errors.New("usage: vili search <query...> [--room ID] [--limit N]")
	}
	ms, err := c.Search(ctx, strings.Join(pos, " "), *room, *limit)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		fmt.Fprintln(out, "no matches")
		return nil
	}
	for _, m := range ms {
		st := c.UI.style()
		fmt.Fprintf(out, "%s  %s  ", st.Time(m.CreatedAt.Local().Format("2006-01-02 15:04")), st.Accent("#"+sanitizeTerminal(m.RoomName)))
		fmt.Fprintf(out, "%s: %s\n", st.User(sanitizeTerminal(m.Username)), highlightTerms(oneLine(sanitizeTerminal(m.Content)), pos, st))
	}
	return nil
}

// highlightTerms emphasizes the plain search words in a result line.
func highlightTerms(line string, terms []string, st *Style) string {
	if !st.Enabled() {
		return line
	}
	lower := strings.ToLower(line)
	if len(lower) != len(line) {
		return line // some runes change width when lowercased; skip rather than misalign
	}
	marks := make([]bool, len(line))
	for _, t := range terms {
		t = strings.ToLower(strings.Trim(t, `"-()`))
		if len(t) < 2 || t == "or" {
			continue
		}
		for from := 0; ; {
			i := strings.Index(lower[from:], t)
			if i < 0 {
				break
			}
			for k := from + i; k < from+i+len(t); k++ {
				marks[k] = true
			}
			from += i + len(t)
		}
	}
	var b strings.Builder
	for i := 0; i < len(line); {
		j := i
		for j < len(line) && marks[j] == marks[i] {
			j++
		}
		if marks[i] {
			b.WriteString(st.Mention(line[i:j]))
		} else {
			b.WriteString(line[i:j])
		}
		i = j
	}
	return b.String()
}

// oneLine collapses newlines so a result stays on a single row.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ---- account -------------------------------------------------------------

func cmdWhoami(ctx context.Context, out io.Writer, c *Client) error {
	u, err := c.Me(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s (id %s) — member since %s — %s\n", c.UI.style().User(sanitizeTerminal(u.Username)), u.ID,
		u.CreatedAt.Local().Format("2006-01-02"), c.BaseURL)
	return nil
}

func cmdPasswd(ctx context.Context, out io.Writer, in io.Reader, c *Client, args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return errors.New("usage: vili passwd <current> <new>   (or pipe 'current' then 'new' on separate stdin lines)")
	}
	cur, next := "", ""
	if len(args) == 2 {
		cur, next = args[0], args[1]
	} else {
		// One arg: current password; the new one is read from stdin.
		cur = args[0]
		var err error
		if next, err = secretArg(in, nil, 0, "new password"); err != nil {
			return err
		}
	}
	resp, err := c.ChangePassword(ctx, cur, next)
	if err != nil {
		return err
	}
	// The server signed out every session; adopt the fresh one for this device.
	c.applySession(resp)
	c.UI.ok(out, "password changed — all other sessions were signed out")
	return nil
}

func cmdDeleteAccount(ctx context.Context, out io.Writer, in io.Reader, c *Client, args []string) error {
	fs := newFlagSet("delete-account")
	yes := fs.Bool("yes", false, "confirm permanent deletion")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) > 1 {
		return errors.New("usage: vili delete-account [<password>] --yes")
	}
	if !*yes {
		return errors.New("this permanently deletes your account and messages; re-run with --yes to confirm")
	}
	pw, err := secretArg(in, pos, 0, "password")
	if err != nil {
		return err
	}
	if err := c.DeleteAccount(ctx, pw); err != nil {
		return err
	}
	_ = ClearSession()
	c.UI.ok(out, "account deleted")
	return nil
}

func cmdSessions(ctx context.Context, out io.Writer, c *Client, args []string) error {
	switch {
	case len(args) == 0:
		ss, err := c.ListSessions(ctx)
		if err != nil {
			return err
		}
		if len(ss) == 0 {
			fmt.Fprintln(out, "no active sessions")
			return nil
		}
		for _, s := range ss {
			fmt.Fprintf(out, "%s  signed in %s  last active %s\n", s.ID,
				s.CreatedAt.Local().Format("2006-01-02 15:04"), s.LastActive.Local().Format("2006-01-02 15:04"))
		}
		return nil
	case len(args) == 2 && args[0] == "revoke":
		if err := c.RevokeSession(ctx, args[1]); err != nil {
			return err
		}
		c.UI.ok(out, "session revoked")
		return nil
	case len(args) == 1 && args[0] == "revoke-all":
		if err := c.RevokeSession(ctx, ""); err != nil {
			return err
		}
		_ = ClearSession() // this device's refresh token is gone too
		c.UI.ok(out, "all sessions revoked — run: vili login")
		return nil
	}
	return errors.New("usage: vili sessions [revoke <session-id> | revoke-all]")
}
