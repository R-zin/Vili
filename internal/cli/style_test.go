package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseHexAnd256(t *testing.T) {
	r, g, b, err := parseHex("#f80")
	if err != nil || r != 0xff || g != 0x88 || b != 0 {
		t.Fatalf("short hex: %d %d %d %v", r, g, b, err)
	}
	for _, bad := range []string{"", "#12", "#gggggg", "12345678"} {
		if _, _, _, err := parseHex(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	cases := map[[3]uint8]int{{255, 0, 0}: 196, {0, 0, 0}: 16, {255, 255, 255}: 231, {128, 128, 128}: 244, {0, 255, 0}: 46}
	for rgb, want := range cases {
		if got := to256(rgb[0], rgb[1], rgb[2]); got != want {
			t.Errorf("to256(%v) = %d want %d", rgb, got, want)
		}
	}
}

func TestDetectColorMode(t *testing.T) {
	var buf bytes.Buffer // not a terminal
	cases := []struct {
		name string
		pref string
		env  map[string]string
		want ColorMode
	}{
		{"auto on a pipe is off", "auto", nil, ColorOff},
		{"never", "never", map[string]string{"COLORTERM": "truecolor"}, ColorOff},
		{"always forces 256", "always", nil, Color256},
		{"always + truecolor", "always", map[string]string{"COLORTERM": "truecolor"}, ColorTrue},
		{"always beats NO_COLOR", "always", map[string]string{"NO_COLOR": "1"}, Color256},
		{"NO_COLOR wins over auto", "", map[string]string{"NO_COLOR": "1"}, ColorOff},
	}
	for _, tc := range cases {
		if got := detectColorMode(&buf, tc.pref, envMap(tc.env)); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestStyleRendering(t *testing.T) {
	var nilStyle *Style
	if nilStyle.Accent("x") != "x" || nilStyle.User("bob") != "bob" || nilStyle.Gradient("hi", 0, "#fff") != "hi" {
		t.Fatal("nil style must pass text through")
	}
	off := NewStyle(ColorOff, themes[0])
	if off.Mention("x") != "x" {
		t.Fatal("ColorOff must pass text through")
	}
	tc := NewStyle(ColorTrue, themes[0])
	if got := tc.Error("boom"); got != "\x1b[38;2;255;95;95m\x1b[1mboom\x1b[0m" {
		t.Fatalf("truecolor error = %q", got)
	}
	c256 := NewStyle(Color256, themes[0])
	if got := c256.Error("boom"); !strings.HasPrefix(got, "\x1b[38;5;") {
		t.Fatalf("256-color error = %q", got)
	}
	// Usernames get a stable color, and the palette spreads people out.
	if tc.User("alice") != tc.User("alice") {
		t.Fatal("user colors must be stable")
	}
	seen := map[string]bool{}
	for _, n := range []string{"alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi"} {
		seen[tc.User(n)[:20]] = true
	}
	if len(seen) < 3 {
		t.Fatalf("user palette too clumped: %d distinct colors", len(seen))
	}
	// Gradients keep the text intact.
	if g := tc.Gradient("vili themes", 0.3, themes[1].Users...); stripANSI(g) != "vili themes" {
		t.Fatalf("gradient altered text: %q", stripANSI(g))
	}
	// Mono themes still emphasize, without colors.
	mono, _ := themeByName("mono")
	ms := NewStyle(ColorTrue, mono)
	if got := ms.Accent("x"); got != "\x1b[1mx\x1b[0m" || strings.Contains(ms.User("bob"), "38;") {
		t.Fatalf("mono should only use weight: %q", got)
	}
	if got := ms.Muted("x"); got != "\x1b[2mx\x1b[0m" {
		t.Fatalf("mono muted should be faint: %q", got)
	}
}

func TestThemesAreValid(t *testing.T) {
	names := map[string]bool{}
	for _, th := range themes {
		if names[th.Name] {
			t.Fatalf("duplicate theme %q", th.Name)
		}
		names[th.Name] = true
		for _, role := range themeRoles {
			hex := *th.role(role)
			if hex == "" && th.Name != "mono" {
				t.Errorf("%s: role %s has no color", th.Name, role)
			}
			if hex != "" {
				if _, _, _, err := parseHex(hex); err != nil {
					t.Errorf("%s.%s: %v", th.Name, role, err)
				}
			}
		}
		for _, u := range th.Users {
			if _, _, _, err := parseHex(u); err != nil {
				t.Errorf("%s user color: %v", th.Name, err)
			}
		}
	}
	if _, ok := themeByName("DRACULA"); !ok {
		t.Fatal("theme lookup should be case-insensitive")
	}
	th, err := themes[0].withOverrides(map[string]string{"mention": "#123456"})
	if err != nil || th.Mention != "#123456" || themes[0].Mention == "#123456" {
		t.Fatalf("overrides: %v %+v", err, th.Mention)
	}
	if _, err := themes[0].withOverrides(map[string]string{"nope": "#fff"}); err == nil {
		t.Fatal("unknown role must be rejected")
	}
	if _, err := themes[0].withOverrides(map[string]string{"accent": "red"}); err == nil {
		t.Fatal("bad hex must be rejected")
	}
}

// Styling must never change what is said, only how it looks, and must never
// let message content smuggle its own escape codes through.
func TestFormatMessage_StyledMatchesPlain(t *testing.T) {
	at := time.Date(2026, 1, 1, 9, 30, 0, 0, time.Local)
	edited := at
	msgs := []Message{
		{ID: "a", Username: "al", Content: "hi @me and @bob", Mentions: []string{"me", "bob"}, CreatedAt: at},
		{ID: "b", Username: "bo\x1b[31mb", Content: "evil \x1b]0;title\x07 content", CreatedAt: at, EditedAt: &edited},
		{ID: "c", Username: "cy", Type: "diff", Content: "@@ -1 +1 @@\n-old\n+new\n--- a\n+++ b", ReplyTo: "0123456789", CreatedAt: at},
		{ID: "d", Username: "di", Type: "code", Content: "func main() {\n\tfmt.Println(1)\n}", CreatedAt: at},
		{ID: "e", Username: "ed", Type: "commit", Content: "fix: thing\n\nbody", CreatedAt: at},
		{ID: "f", Username: "fi", Type: "log", Content: "single line log", CreatedAt: at},
		{ID: "g", Username: "gi", Deleted: true, Content: "", CreatedAt: at},
	}
	styled := printOpts{Me: "me", IDs: true, Style: NewStyle(ColorTrue, themes[1]), TimeLayout: "15:04:05"}
	plain := printOpts{Me: "me", IDs: true}
	for _, m := range msgs {
		s, p := formatMessage(m, styled), formatMessage(m, plain)
		if stripANSI(s) != p {
			t.Errorf("%s: styled text differs from plain:\nstyled: %q\nplain:  %q", m.ID, stripANSI(s), p)
		}
		if strings.Contains(p, "\x1b") || strings.Contains(stripANSI(s), "\x1b") || strings.Contains(s, "\x07") {
			t.Errorf("%s: escape codes leaked from content: %q", m.ID, s)
		}
	}
	// Multi-line payloads get a gutter even in plain mode.
	diff := formatMessage(msgs[2], plain)
	if !strings.Contains(diff, "\n  │ +new") || !strings.Contains(diff, "\n  │ -old") {
		t.Fatalf("diff gutter missing:\n%s", diff)
	}
	// Time formats.
	o := printOpts{Style: NewStyle(ColorOff, themes[0]), TimeLayout: "3:04 PM"}
	if got := formatMessage(msgs[0], o); !strings.HasPrefix(got, "9:30 AM  al: ") {
		t.Fatalf("12h layout: %q", got)
	}
	o.TimeLayout = ""
	if got := formatMessage(msgs[0], o); !strings.HasPrefix(got, "al: ") {
		t.Fatalf("time-format off: %q", got)
	}
}

func TestPrefsSetUnsetAndPersist(t *testing.T) {
	isolateConfig(t)
	var p Prefs
	for _, kv := range [][2]string{{"theme", "Nord"}, {"color", "always"}, {"animations", "off"}, {"bell", "on"}, {"time-format", "12h"}, {"color.mention", "#FF00FF"}} {
		if err := p.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
	}
	for _, bad := range [][2]string{{"theme", "nope"}, {"color", "sometimes"}, {"animations", "maybe"}, {"time-format", "iso"}, {"color.nope", "#fff"}, {"color.accent", "blue"}, {"wat", "1"}} {
		if err := p.Set(bad[0], bad[1]); err == nil {
			t.Errorf("set %v should fail", bad)
		}
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPrefs()
	if err != nil || got.Theme != "nord" || got.Color != "always" || *got.Animations || !*got.Bell || got.Colors["mention"] != "#ff00ff" {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	entries := map[string]string{}
	for _, kv := range got.Entries() {
		entries[kv[0]] = kv[1]
	}
	if entries["animations"] != "off" || entries["color.mention"] != "#ff00ff" || entries["time-format"] != "12h" {
		t.Fatalf("entries: %v", entries)
	}
	for _, k := range []string{"theme", "color", "animations", "bell", "time-format", "color.mention"} {
		if err := got.Unset(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
	if got.Theme != "" || got.Animations != nil || got.Colors != nil || got.Unset("wat") == nil {
		t.Fatalf("unset left state: %+v", got)
	}
	if def := (Prefs{}).Entries(); def[0][1] != "classic" || def[2][1] != "on" || def[3][1] != "off" {
		t.Fatalf("defaults: %v", def)
	}
}

func TestNewUI(t *testing.T) {
	var buf bytes.Buffer
	u, err := newUI(&buf, Prefs{Theme: "gruvbox", TimeFormat: "short"}, uiOverrides{}, envMap(nil))
	if err != nil || u.Style.Theme().Name != "gruvbox" || u.Style.Enabled() || u.TimeLayout != "15:04" || !u.Anim {
		t.Fatalf("ui: %v %+v", err, u)
	}
	u, _ = newUI(&buf, Prefs{Theme: "gruvbox"}, uiOverrides{Theme: "nord", Color: "always", NoAnim: true}, envMap(nil))
	if u.Style.Theme().Name != "nord" || !u.Style.Enabled() || u.Anim {
		t.Fatal("flags must override prefs")
	}
	u, _ = newUI(&buf, Prefs{}, uiOverrides{}, envMap(map[string]string{"VILI_THEME": "matrix", "VILI_COLOR": "always", "NO_COLOR": "1"}))
	if u.Style.Theme().Name != "matrix" || !u.Style.Enabled() || u.Anim {
		t.Fatal("env must be honored (NO_COLOR also stops animation)")
	}
	if _, err := newUI(&buf, Prefs{}, uiOverrides{Theme: "nope"}, envMap(nil)); err == nil {
		t.Fatal("unknown theme flag must fail")
	}
	if _, err := newUI(&buf, Prefs{}, uiOverrides{Color: "rainbow"}, envMap(nil)); err == nil {
		t.Fatal("bad --color must fail")
	}
	// Broken saved settings degrade with warnings instead of failing.
	u, err = newUI(&buf, Prefs{Theme: "gone", Color: "loud", Colors: map[string]string{"accent": "nope"}}, uiOverrides{}, envMap(nil))
	if err != nil || u.Style.Theme().Name != "classic" || len(u.Warnings) != 3 {
		t.Fatalf("bad saved settings should warn and fall back: %v %v", err, u.Warnings)
	}
	if u.Style.Theme().Accent != themes[0].Accent {
		t.Fatal("invalid overrides must be ignored")
	}
}

// lockedBuffer is a goroutine-safe buffer for capturing spinner output.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestSpinner(t *testing.T) {
	var out lockedBuffer
	u := &UI{Anim: true, ErrTTY: true, Err: &out, ErrStyle: NewStyle(ColorTrue, themes[0])}

	// A fast request never draws anything.
	u.Spin("quick")()
	if out.String() != "" {
		t.Fatalf("fast request drew a spinner: %q", out.String())
	}

	stop := u.Spin("loading history")
	time.Sleep(spinDelay + 3*spinInterval)
	stop()
	stop() // idempotent
	got := out.String()
	if !strings.Contains(stripANSI(got), "loading history") || !strings.Contains(got, "⠋") {
		t.Fatalf("spinner frames missing: %q", got)
	}
	if !strings.HasPrefix(got, "\x1b[?25l") || !strings.HasSuffix(got, "\r\x1b[K\x1b[?25h") {
		t.Fatalf("spinner must hide the cursor and fully clean up: %q", got)
	}

	// Disabled cases are silent.
	for _, quiet := range []*UI{nil, {Anim: false, ErrTTY: true, Err: &out}, {Anim: true, ErrTTY: false, Err: &out}} {
		before := out.String()
		s := quiet.Spin("x")
		time.Sleep(spinDelay + spinInterval)
		s()
		if out.String() != before {
			t.Fatal("spinner must be silent when animations or the terminal are unavailable")
		}
	}
}

func TestBannerAndTypewrite(t *testing.T) {
	var buf bytes.Buffer
	var u *UI
	u.Banner(&buf, "plain title")
	u.Typewrite(&buf, "plain line")
	if buf.String() != "plain title\nplain line\n" {
		t.Fatalf("plain banner/typewrite: %q", buf.String())
	}
	buf.Reset()
	colored := &UI{Style: NewStyle(ColorTrue, themes[0]), Anim: true, OutTTY: true}
	colored.Banner(&buf, "◆ vili · general")
	got := buf.String()
	if strings.Count(got, "\r\x1b[K") < bannerFrames || !strings.HasSuffix(stripANSI(got), "◆ vili · general\n") {
		t.Fatalf("animated banner: %q", got)
	}
	buf.Reset()
	colored.Typewrite(&buf, colored.Style.Success("✓")+" done")
	if stripANSI(buf.String()) != "✓ done\n" {
		t.Fatalf("typewrite: %q", buf.String())
	}
}

func TestPickerModel(t *testing.T) {
	keys := map[string]string{"\x1b[A": keyUp, "k": keyUp, "\x1b[B": keyDown, "j": keyDown, "\r": keyEnter, "q": keyCancel, "\x1b": keyCancel, "\x03": keyCancel, "3": "3", "x": ""}
	for raw, want := range keys {
		if got := decodeKey([]byte(raw)); got != want {
			t.Errorf("decodeKey(%q) = %q want %q", raw, got, want)
		}
	}
	p := &picker{}
	p.handle(keyUp) // wraps to the last theme
	if p.idx != len(themes)-1 {
		t.Fatalf("wrap up: %d", p.idx)
	}
	p.handle(keyDown)
	p.handle("3")
	if p.idx != 2 {
		t.Fatalf("jump: %d", p.idx)
	}
	p.handle(fmt.Sprint(len(themes) + 1)) // beyond the list (or a non-digit) is ignored
	p.handle("0")
	if p.idx != 2 {
		t.Fatalf("out-of-range jump moved selection: %d", p.idx)
	}
	frame := p.render(ColorTrue, 0.5)
	plain := stripANSI(frame)
	for _, th := range themes {
		if !strings.Contains(plain, th.Name) {
			t.Errorf("frame missing theme %s", th.Name)
		}
	}
	if !strings.Contains(plain, "❯ "+themes[p.idx].Name) || !strings.Contains(plain, "deploy is green") {
		t.Fatalf("selection/preview missing:\n%s", plain)
	}
	if strings.Contains(strings.ReplaceAll(frame, "\r\n", ""), "\n") {
		t.Fatal("raw-mode frames must use CRLF line endings")
	}
	p.handle(keyEnter)
	if !p.done || p.cancelled {
		t.Fatal("enter should finish")
	}
	p2 := &picker{}
	p2.handle(keyCancel)
	if !p2.cancelled {
		t.Fatal("q should cancel")
	}
}

func TestRenderUsage(t *testing.T) {
	st := NewStyle(ColorTrue, themes[0])
	if renderUsage(nil) != usage {
		t.Fatal("plain usage must be unchanged")
	}
	colored := renderUsage(st)
	if colored == usage || stripANSI(colored) != usage {
		t.Fatal("colored usage must only add styling")
	}
}

func TestRenderEvent_BellAndStyle(t *testing.T) {
	var buf bytes.Buffer
	p, _ := json.Marshal(Message{Username: "al", Content: "hey @me", Mentions: []string{"me"}, CreatedAt: time.Now()})
	renderEvent(&buf, wsEvent{Type: eventMessageNew, Payload: p}, printOpts{Me: "me"}, true)
	if !strings.HasSuffix(buf.String(), "\a") || !strings.HasPrefix(buf.String(), "» ") {
		t.Fatalf("mention should be flagged and ring: %q", buf.String())
	}
	buf.Reset()
	renderEvent(&buf, wsEvent{Type: eventMessageNew, Payload: p}, printOpts{Me: "other"}, true)
	if strings.Contains(buf.String(), "\a") {
		t.Fatal("no bell for messages that don't mention you")
	}
	buf.Reset()
	j, _ := json.Marshal(map[string]string{"username": "bob"})
	renderEvent(&buf, wsEvent{Type: eventPresenceJoin, Payload: j}, printOpts{Style: NewStyle(ColorTrue, themes[0])}, false)
	if !strings.Contains(buf.String(), "\x1b[") || stripANSI(buf.String()) != "  → bob joined\n" {
		t.Fatalf("styled presence: %q", buf.String())
	}
}

// ---- end-to-end through Run ---------------------------------------------------

func TestRun_ColorFlagsAndThemeCommands(t *testing.T) {
	isolateConfig(t)
	rec, url := newRecorder(t)
	rec.on("GET /v1/listrooms", []Room{{ID: "r1", Name: "general", Unread: 2, Role: "owner"}})

	out, err := run(t, url, "", "rooms")
	if err != nil || strings.Contains(out, "\x1b") {
		t.Fatalf("pipes must be plain: %v %q", err, out)
	}
	out, err = run(t, url, "", "--color", "always", "--theme", "dracula", "rooms")
	if err != nil || !strings.Contains(out, "\x1b[") || stripANSI(out) != "r1  general  [owner, 2 unread]\n" {
		t.Fatalf("--color always: %v %q", err, out)
	}
	if _, err := run(t, url, "", "--color", "rainbow", "rooms"); err == nil {
		t.Fatal("bad --color must fail")
	}

	// Theme commands work without a server or session.
	var buf bytes.Buffer
	if err := Run(t.Context(), &buf, strings.NewReader(""), []string{"theme", "set", "nord"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "theme set to nord") {
		t.Fatalf("theme set: %q", buf.String())
	}
	buf.Reset()
	_ = Run(t.Context(), &buf, strings.NewReader(""), []string{"theme"}) // non-interactive → list
	if !strings.Contains(buf.String(), "* nord") || strings.Count(buf.String(), "\n") != len(themes) {
		t.Fatalf("theme list should mark the saved theme:\n%s", buf.String())
	}
	buf.Reset()
	if err := Run(t.Context(), &buf, strings.NewReader(""), []string{"--color", "always", "theme", "preview", "matrix"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripANSI(buf.String()), "matrix") || !strings.Contains(buf.String(), "38;5;") {
		t.Fatalf("preview: %q", buf.String())
	}
	for _, bad := range [][]string{{"theme", "set", "nope"}, {"theme", "preview", "nope"}, {"theme", "bogus", "x", "y"}} {
		if err := Run(t.Context(), &buf, strings.NewReader(""), bad); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}

	// config command.
	buf.Reset()
	for _, args := range [][]string{{"config", "set", "time-format", "short"}, {"config", "set", "color.accent", "#00ff00"}, {"config", "unset", "color.accent"}} {
		if err := Run(t.Context(), &buf, strings.NewReader(""), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	buf.Reset()
	_ = Run(t.Context(), &buf, strings.NewReader(""), []string{"config"})
	if !strings.Contains(buf.String(), "time-format") || !strings.Contains(buf.String(), "short") || strings.Contains(buf.String(), "#00ff00") {
		t.Fatalf("config show: %s", buf.String())
	}
	if err := Run(t.Context(), &buf, strings.NewReader(""), []string{"config", "set", "color", "loud"}); err == nil {
		t.Fatal("invalid config value must fail")
	}
	buf.Reset()
	_ = Run(t.Context(), &buf, strings.NewReader(""), []string{"config", "path"})
	if !strings.HasSuffix(strings.TrimSpace(buf.String()), filepath.Join("vili", "config.json")) {
		t.Fatalf("config path: %q", buf.String())
	}

	// The saved time format reaches history output.
	rec.on("GET /v1/rooms/r1/messages", []Message{{ID: "m", Username: "al", Content: "x", CreatedAt: time.Date(2026, 1, 1, 7, 5, 9, 0, time.Local)}})
	out, _ = run(t, url, "", "history", "r1")
	if out != "07:05  al: x\n" {
		t.Fatalf("time-format short not applied: %q", out)
	}
	if err := Run(t.Context(), &buf, strings.NewReader(""), []string{"config", "reset"}); err != nil {
		t.Fatal(err)
	}
}

func TestRun_CorruptConfigStillWorks(t *testing.T) {
	isolateConfig(t)
	path, _ := prefsPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(`{"theme":"deleted-theme","colors":{"accent":"blue"}}`), 0o600)
	var errOut0 bytes.Buffer
	old0 := stderr
	stderr = &errOut0
	var b0 bytes.Buffer
	if err := Run(t.Context(), &b0, strings.NewReader(""), []string{"config"}); err != nil {
		t.Fatalf("invalid saved values must not brick the CLI: %v", err)
	}
	stderr = old0
	if !strings.Contains(errOut0.String(), "unknown theme") {
		t.Fatalf("expected a warning about the theme: %q", errOut0.String())
	}
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	var errOut bytes.Buffer
	old := stderr
	stderr = &errOut
	t.Cleanup(func() { stderr = old })

	var buf bytes.Buffer
	if err := Run(t.Context(), &buf, strings.NewReader(""), []string{"config", "reset"}); err != nil {
		t.Fatalf("reset must work with a corrupt config: %v", err)
	}
	if !strings.Contains(errOut.String(), "warning") {
		t.Fatalf("expected a warning, got %q", errOut.String())
	}
	if p, err := LoadPrefs(); err != nil || p.Theme != "" {
		t.Fatalf("reset should leave a valid default config: %v", err)
	}
}

func TestPrintError(t *testing.T) {
	isolateConfig(t)
	var errOut bytes.Buffer
	old := stderr
	stderr = &errOut
	t.Cleanup(func() { stderr = old })
	PrintError(os.ErrNotExist)
	if errOut.String() != "vili: file does not exist\n" {
		t.Fatalf("plain error: %q", errOut.String())
	}
}
