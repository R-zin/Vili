package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a fake backend that records every request and answers from a
// route table keyed by "METHOD /path".
type recorder struct {
	mu     sync.Mutex
	calls  []call
	routes map[string]func(w http.ResponseWriter, body map[string]any)
}

type call struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

func newRecorder(t *testing.T) (*recorder, string) {
	t.Helper()
	rec := &recorder{routes: map[string]func(http.ResponseWriter, map[string]any){}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, call{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), body})
		fn := rec.routes[r.Method+" "+r.URL.EscapedPath()]
		rec.mu.Unlock()
		if fn == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "not_found", "message": "no such route in fake"}})
			return
		}
		fn(w, body)
	}))
	t.Cleanup(srv.Close)
	return rec, srv.URL
}

func (r *recorder) on(route string, v any) {
	r.routes[route] = func(w http.ResponseWriter, _ map[string]any) { _ = json.NewEncoder(w).Encode(v) }
}

func (r *recorder) last() call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

// run executes the CLI against server with a saved fake session.
func run(t *testing.T, server, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	full := append([]string{"--token", "tok", "--server", server}, args...)
	err := Run(context.Background(), &out, strings.NewReader(stdin), full)
	return out.String(), err
}

func TestParseFlags_Interspersed(t *testing.T) {
	fs := newFlagSet("x")
	typ := fs.String("type", "", "")
	pos, err := parseFlags(fs, []string{"room", "hello", "--type", "code", "world"})
	if err != nil || *typ != "code" || strings.Join(pos, "|") != "room|hello|world" {
		t.Fatalf("pos=%v typ=%q err=%v", pos, *typ, err)
	}
	// After "--" everything is positional, even flag-looking text.
	fs = newFlagSet("x")
	typ = fs.String("type", "", "")
	pos, _ = parseFlags(fs, []string{"room", "--type", "log", "--", "--type", "-x"})
	if *typ != "log" || strings.Join(pos, "|") != "room|--type|-x" {
		t.Fatalf("pos=%v typ=%q", pos, *typ)
	}
	if _, err := parseFlags(newFlagSet("x"), []string{"--bogus"}); err == nil {
		t.Fatal("unknown flag must error")
	}
}

func TestSendVariants(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("POST /v1/rooms/r1/messages", Message{ID: "m1"})

	if _, err := run(t, url, "", "send", "r1", "hi", "there", "--type", "code", "--reply", "abc"); err != nil {
		t.Fatal(err)
	}
	if c := rec.last(); c.Body["content"] != "hi there" || c.Body["type"] != "code" || c.Body["reply_to"] != "abc" || c.Auth != "Bearer tok" {
		t.Fatalf("send body: %+v", c)
	}

	// --file from disk and from stdin; trailing newline trimmed.
	path := filepath.Join(t.TempDir(), "d.diff")
	_ = os.WriteFile(path, []byte("+added line\n"), 0o600)
	if _, err := run(t, url, "", "send", "r1", "--file", path, "--type", "diff"); err != nil {
		t.Fatal(err)
	}
	if c := rec.last(); c.Body["content"] != "+added line" || c.Body["type"] != "diff" {
		t.Fatalf("file body: %+v", c)
	}
	if _, err := run(t, url, "from stdin\n", "send", "r1", "--file", "-"); err != nil {
		t.Fatal(err)
	}
	if c := rec.last(); c.Body["content"] != "from stdin" {
		t.Fatalf("stdin body: %+v", c)
	}

	// Local validation never reaches the server.
	n := len(rec.calls)
	for _, bad := range [][]string{
		{"send", "r1"},
		{"send", "r1", "--file", "/definitely/missing"},
		{"send", "r1", strings.Repeat("x", 4001)},
		{"send", "r1", "   "},
	} {
		if _, err := run(t, url, "", bad...); err == nil {
			t.Errorf("%v should fail", bad[:2])
		}
	}
	if len(rec.calls) != n {
		t.Fatal("invalid sends must not hit the server")
	}
}

func TestRoomsListing(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("GET /v1/listrooms", []Room{
		{ID: "1", Name: "general", Role: "owner", Unread: 3, UnreadMentions: 1, Description: "hello"},
		{ID: "2", Name: "secret", IsPrivate: true, Role: "member"},
		{ID: "3", Name: "dm:x", IsDM: true, Peer: "bob", Unread: 1},
		{ID: "4", Name: "evil\x1b[31m"},
	})
	out, err := run(t, url, "", "rooms", "--mine", "--search", "gen")
	if err != nil {
		t.Fatal(err)
	}
	if c := rec.last(); c.Query != "mine=1&q=gen" {
		t.Fatalf("query = %q", c.Query)
	}
	for _, want := range []string{"[owner, 3 unread, 1 @you]", "[private]", "@bob", "[dm, 1 unread]", "— hello"} {
		if !strings.Contains(out, want) {
			t.Errorf("rooms output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("room names must be sanitized")
	}

	out, _ = run(t, url, "", "unread")
	if strings.Contains(out, "general  ") && !strings.Contains(out, "3 unread") || strings.Contains(out, "secret") {
		t.Fatalf("unread should list only rooms with unread: %s", out)
	}
	rec.on("GET /v1/listrooms", []Room{{ID: "1", Name: "a"}})
	if out, _ := run(t, url, "", "unread"); !strings.Contains(out, "all caught up") {
		t.Fatalf("expected caught-up message, got %q", out)
	}
}

func TestMembershipCommands(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("GET /v1/rooms/r1/members", []Member{{UserID: "u-bob", Username: "bob", Role: "member"}, {UserID: "u-al", Username: "al", Role: "owner"}})
	rec.on("DELETE /v1/rooms/r1/members/u-bob", map[string]string{"status": "removed"})
	rec.on("PUT /v1/rooms/r1/members/u-bob/role", map[string]string{"status": "updated"})

	out, err := run(t, url, "", "members", "r1")
	if err != nil || !strings.Contains(out, "owner") || !strings.Contains(out, "bob") {
		t.Fatalf("members: %v %s", err, out)
	}
	if _, err := run(t, url, "", "kick", "r1", "@bob"); err != nil {
		t.Fatal(err)
	}
	if c := rec.last(); c.Method != "DELETE" || c.Path != "/v1/rooms/r1/members/u-bob" {
		t.Fatalf("kick: %+v", c)
	}
	if _, err := run(t, url, "", "promote", "r1", "bob"); err != nil || rec.last().Body["role"] != "admin" {
		t.Fatalf("promote: %v %+v", err, rec.last())
	}
	if _, err := run(t, url, "", "demote", "r1", "bob"); err != nil || rec.last().Body["role"] != "member" {
		t.Fatalf("demote: %v %+v", err, rec.last())
	}
	if _, err := run(t, url, "", "kick", "r1", "ghost"); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("unknown member: %v", err)
	}
}

func TestInviteCommands(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	exp := time.Now().Add(time.Hour)
	rec.on("POST /v1/rooms/r1/invites", createInviteResponse{Code: "abc123", Invite: Invite{ID: "i1", ExpiresAt: exp}})
	rec.on("GET /v1/rooms/r1/invites", []Invite{
		{ID: "i1", ExpiresAt: exp, MaxUses: 2, Uses: 1},
		{ID: "i2", ExpiresAt: exp, MaxUses: 1, Uses: 1},
		{ID: "i3", ExpiresAt: exp.Add(-2 * time.Hour)},
		{ID: "i4", ExpiresAt: exp, RevokedAt: &exp},
	})
	rec.on("DELETE /v1/rooms/r1/invites/i1", map[string]string{"status": "revoked"})
	rec.on("POST /v1/invites/redeem", Room{ID: "r1", Name: "vault"})

	out, err := run(t, url, "", "invite", "r1", "--uses", "3", "--ttl-hours", "12")
	if err != nil || !strings.Contains(out, "abc123") {
		t.Fatalf("invite: %v %s", err, out)
	}
	if c := rec.last(); c.Body["max_uses"] != float64(3) || c.Body["ttl_hours"] != float64(12) {
		t.Fatalf("invite body: %+v", c.Body)
	}
	out, _ = run(t, url, "", "invites", "r1")
	for _, want := range []string{"active", "used up", "expired", "revoked", "1/2", "1/1"} {
		if !strings.Contains(out, want) {
			t.Errorf("invites output missing %q:\n%s", want, out)
		}
	}
	if _, err := run(t, url, "", "revoke-invite", "r1", "i1"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, url, "", "redeem", "abc123")
	if err != nil || !strings.Contains(out, "joined vault") || rec.last().Body["code"] != "abc123" {
		t.Fatalf("redeem: %v %s %+v", err, out, rec.last())
	}
}

func TestRoomEditDeleteDM(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("PATCH /v1/rooms/r1", Room{ID: "r1", Name: "renamed"})
	rec.on("DELETE /v1/rooms/r1", map[string]string{"status": "deleted"})
	rec.on("POST /v1/dms", Room{ID: "d1", IsDM: true})
	rec.on("POST /v1/rooms/r1/read", map[string]string{"status": "read"})
	rec.on("POST /v1/createroom", Room{ID: "r9", Name: "s", IsPrivate: true})

	if _, err := run(t, url, "", "edit-room", "r1", "--name", "renamed", "--private", "false"); err != nil {
		t.Fatal(err)
	}
	if b := rec.last().Body; b["name"] != "renamed" || b["is_private"] != false || b["description"] != nil {
		t.Fatalf("only passed fields may be sent: %+v", b)
	}
	for _, bad := range [][]string{{"edit-room", "r1"}, {"edit-room", "r1", "--private", "maybe"}, {"delete-room", "r1"}} {
		if _, err := run(t, url, "", bad...); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
	if _, err := run(t, url, "", "delete-room", "r1", "--yes"); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, url, "", "dm", "@bob"); err != nil || !strings.Contains(out, "d1") || rec.last().Body["username"] != "bob" {
		t.Fatalf("dm: %v %s %+v", err, out, rec.last())
	}
	if _, err := run(t, url, "", "read", "r1"); err != nil || rec.last().Path != "/v1/rooms/r1/read" {
		t.Fatalf("read: %v %+v", err, rec.last())
	}
	if out, err := run(t, url, "", "create", "s", "--private", "desc"); err != nil || !strings.Contains(out, "(private)") || rec.last().Body["is_private"] != true {
		t.Fatalf("create private: %v %s %+v", err, out, rec.last())
	}
}

func TestMessageCommands(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("PATCH /v1/rooms/r1/messages/m1", Message{ID: "m1"})
	rec.on("DELETE /v1/rooms/r1/messages/m1", map[string]string{"status": "deleted"})
	rec.on("GET /v1/search", []Message{{Username: "al", Content: "a\nmulti line\x1b[0m hit", RoomName: "dev", CreatedAt: time.Now()}})

	if _, err := run(t, url, "", "edit", "r1", "m1", "new", "text"); err != nil || rec.last().Body["content"] != "new text" {
		t.Fatalf("edit: %v %+v", err, rec.last())
	}
	if _, err := run(t, url, "", "delete", "r1", "m1"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, url, "", "search", "multi", "line", "--room", "r1", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	if q := rec.last().Query; q != "limit=5&q=multi+line&room_id=r1" {
		t.Fatalf("search query: %s", q)
	}
	if strings.Count(out, "\n") != 1 || strings.Contains(out, "\x1b") || !strings.Contains(out, "#dev") {
		t.Fatalf("search output must be one sanitized line: %q", out)
	}
	rec.on("GET /v1/search", []Message{})
	if out, _ := run(t, url, "", "search", "zzz"); !strings.Contains(out, "no matches") {
		t.Fatalf("empty search: %q", out)
	}
	for _, bad := range [][]string{{"edit", "r1", "m1"}, {"edit", "r1", "m1", " "}, {"delete", "r1"}, {"search"}} {
		if _, err := run(t, url, "", bad...); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
}

func TestPathInjectionIsEscaped(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	_, _ = run(t, url, "", "delete", "../admin", "x/y")
	if p := rec.last().Path; p != "/v1/rooms/..%2Fadmin/messages/x%2Fy" {
		t.Fatalf("ids must be escaped as single segments, got %s", p)
	}
}

func TestAccountCommands(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("GET /v1/me", User{ID: "u1", Username: "al", CreatedAt: time.Now()})
	rec.on("GET /v1/me/sessions", []SessionInfo{{ID: "s1", CreatedAt: time.Now(), LastActive: time.Now()}})
	rec.on("DELETE /v1/me/sessions/s1", map[string]string{"status": "revoked"})
	rec.on("DELETE /v1/me/sessions", map[string]string{"status": "revoked"})
	rec.on("DELETE /v1/me", map[string]string{"status": "deleted"})
	rec.on("POST /v1/me/password", loginResponse{Token: "t2", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour)})

	if out, err := run(t, url, "", "whoami"); err != nil || !strings.Contains(out, "al") {
		t.Fatalf("whoami: %v %s", err, out)
	}
	if out, err := run(t, url, "", "sessions"); err != nil || !strings.Contains(out, "s1") {
		t.Fatalf("sessions: %v %s", err, out)
	}
	if _, err := run(t, url, "", "sessions", "revoke", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, url, "", "sessions", "revoke-all"); err != nil || rec.last().Path != "/v1/me/sessions" {
		t.Fatalf("revoke-all: %v %+v", err, rec.last())
	}
	if _, err := run(t, url, "", "sessions", "bogus"); err == nil {
		t.Fatal("bad sessions usage must fail")
	}

	if _, err := run(t, url, "", "passwd", "old", "newpassword"); err != nil {
		t.Fatal(err)
	}
	if b := rec.last().Body; b["current_password"] != "old" || b["new_password"] != "newpassword" {
		t.Fatalf("passwd body: %+v", b)
	}
	// New password piped on stdin when only the current one is an arg.
	if _, err := run(t, url, "piped-new\n", "passwd", "old"); err != nil || rec.last().Body["new_password"] != "piped-new" {
		t.Fatalf("piped passwd: %v %+v", err, rec.last())
	}

	if _, err := run(t, url, "", "delete-account", "pw"); err == nil {
		t.Fatal("delete-account without --yes must refuse")
	}
	if _, err := run(t, url, "pw\n", "delete-account", "--yes"); err != nil || rec.last().Body["password"] != "pw" {
		t.Fatalf("delete-account: %v %+v", err, rec.last())
	}
}

func TestLoginPasswordFromStdin(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("POST /v1/auth/login", loginResponse{Token: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour), User: User{Username: "dev"}})
	rec.on("POST /v1/auth/register", User{ID: "1", Username: "dev"})
	if _, err := run(t, url, "s3cret-pass\n", "login", "dev"); err != nil || rec.last().Body["password"] != "s3cret-pass" {
		t.Fatalf("login via stdin: %v %+v", err, rec.last())
	}
	if _, err := run(t, url, "", "login", "dev"); err == nil {
		t.Fatal("no password anywhere must fail")
	}
	if _, err := run(t, url, "pw12345678\n", "register", "dev"); err != nil || rec.last().Body["password"] != "pw12345678" {
		t.Fatalf("register via stdin: %v", err)
	}
}

func TestFormatMessage(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	edited := ts
	cases := []struct {
		name string
		m    Message
		o    printOpts
		want []string
		not  []string
	}{
		{"plain", Message{Username: "al", Content: "hi", CreatedAt: ts}, printOpts{}, []string{"al: hi"}, []string{"edited", "»"}},
		{"edited", Message{Username: "al", Content: "hi", CreatedAt: ts, EditedAt: &edited}, printOpts{}, []string{"hi (edited)"}, nil},
		{"deleted", Message{Username: "al", Content: "secret?", Deleted: true, CreatedAt: ts}, printOpts{}, []string{"[message deleted]"}, []string{"secret"}},
		{"reply", Message{Username: "al", Content: "x", ReplyTo: "0123456789abcdef", CreatedAt: ts}, printOpts{}, []string{"(↪ 01234567)"}, nil},
		{"mention me", Message{Username: "al", Content: "yo", Mentions: []string{"me"}, CreatedAt: ts}, printOpts{Me: "me"}, []string{"» "}, nil},
		{"mention other", Message{Username: "al", Content: "yo", Mentions: []string{"you"}, CreatedAt: ts}, printOpts{Me: "me"}, nil, []string{"»"}},
		{"ids", Message{ID: "full-id", Username: "al", Content: "x", CreatedAt: ts}, printOpts{IDs: true}, []string{"full-id  "}, nil},
		{"injection", Message{Username: "a\x1b[2Jl", Content: "b\x1b]0;pwn\x07ad", CreatedAt: ts}, printOpts{}, []string{"al: bad"}, []string{"\x1b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatMessage(tc.m, tc.o)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("unexpected %q in %q", n, got)
				}
			}
		})
	}
}

func TestRenderEvent_NewTypes(t *testing.T) {
	var out bytes.Buffer
	edited := time.Now()
	p, _ := json.Marshal(Message{Username: "al", Content: "fixed", EditedAt: &edited, CreatedAt: time.Now()})
	renderEvent(&out, wsEvent{Type: eventMessageEdited, Payload: p}, "")
	d, _ := json.Marshal(map[string]string{"id": "0123456789abcdef"})
	renderEvent(&out, wsEvent{Type: eventMessageDeleted, Payload: d}, "")
	for _, w := range []string{"edited:", "fixed (edited)", "message 01234567 was deleted"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("missing %q in %q", w, out.String())
		}
	}
	// Malformed payloads must not panic or print.
	out.Reset()
	renderEvent(&out, wsEvent{Type: eventMessageEdited, Payload: []byte("nope")}, "")
	renderEvent(&out, wsEvent{Type: eventMessageDeleted, Payload: []byte(`{}`)}, "")
	if out.Len() != 0 {
		t.Fatalf("expected silence, got %q", out.String())
	}
}

func TestHistoryIDsAndCursorPrecision(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("GET /v1/rooms/r1/messages", []Message{{ID: "mid-1", Username: "al", Content: "x", CreatedAt: time.Now()}})
	out, err := run(t, url, "", "history", "r1", "--ids", "--limit", "5", "--before", "2026-01-01T00:00:00.123456Z")
	if err != nil || !strings.HasPrefix(out, "mid-1  ") {
		t.Fatalf("history --ids: %v %q", err, out)
	}
	if q := rec.last().Query; !strings.Contains(q, "before=2026-01-01T00%3A00%3A00.123456Z") {
		t.Fatalf("sub-second precision must survive: %s", q)
	}
}

func TestSecretArgAndReadContentErrors(t *testing.T) {
	if _, err := secretArg(nil, nil, 0, "password"); err == nil {
		t.Fatal("nil stdin must error")
	}
	if v, _ := secretArg(strings.NewReader("a\r\nb"), nil, 0, "x"); v != "a" {
		t.Fatalf("first line only, CR trimmed: %q", v)
	}
	if _, err := secretArg(io.NopCloser(strings.NewReader("")), nil, 0, "x"); err == nil {
		t.Fatal("empty stdin must error")
	}
	big := strings.Repeat("y", 5000)
	got, _ := readContent(strings.NewReader(big), "-")
	if len(got) != maxContentBytes+1 {
		t.Fatalf("read must be bounded, got %d", len(got))
	}
}
