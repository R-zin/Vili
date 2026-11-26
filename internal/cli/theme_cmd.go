package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// sampleMessages is the fixed conversation used to preview a theme.
func sampleMessages() []Message {
	at := time.Date(2026, 1, 1, 9, 41, 0, 0, time.Local)
	edited := at
	return []Message{
		{ID: "0f3c9a1e", Username: "alice", Content: "deploy is green — nice work @you", Mentions: []string{"you"}, CreatedAt: at},
		{ID: "7d21b0c4", Username: "bob", Type: "diff", Content: "@@ -1,2 +1,2 @@\n-timeout = 5s\n+timeout = 30s", ReplyTo: "0f3c9a1e", CreatedAt: at.Add(time.Minute)},
		{ID: "a9e04d77", Username: "carol", Content: "reverting the flaky test", EditedAt: &edited, CreatedAt: at.Add(2 * time.Minute)},
		{ID: "c0ffee00", Username: "dave", Deleted: true, CreatedAt: at.Add(3 * time.Minute)},
	}
}

// renderPreview renders the sample conversation and a status line in st.
func renderPreview(st *Style) []string {
	o := printOpts{Me: "you", Style: st, TimeLayout: "15:04"}
	var lines []string
	for _, m := range sampleMessages() {
		lines = append(lines, strings.Split(formatMessage(m, o), "\n")...)
	}
	lines = append(lines,
		"  "+st.System("· erin is typing…"),
		"  "+st.Success("✓ sent")+"  "+st.Warn("3 unread")+"  "+st.Mention("1 @you")+"  "+st.Error("✗ failed"))
	return lines
}

// swatches draws one colored block per role, for theme listings.
func swatches(st *Style) string {
	t := st.Theme()
	var b strings.Builder
	for _, hex := range []string{t.Accent, t.Mention, t.Success, t.Warn, t.Error, t.System, t.Code} {
		if hex == "" {
			b.WriteString(st.Bold("■"))
		} else {
			b.WriteString(st.paint("■", hex))
		}
	}
	for _, hex := range t.Users {
		b.WriteString(st.paint("●", hex))
	}
	return b.String()
}

// currentTheme is the theme name in effect for u.
func currentTheme(u *UI) string { return u.style().Theme().Name }

func cmdTheme(out io.Writer, in io.Reader, u *UI, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "" && interactive(in, out):
		return runThemePicker(in.(*os.File), out, u)
	case sub == "" || sub == "list":
		return themeList(out, u)
	case sub == "set" && len(args) == 2:
		return saveTheme(out, u, args[1])
	case sub == "preview" && len(args) <= 2:
		name := currentTheme(u)
		if len(args) == 2 {
			name = args[1]
		}
		t, ok := themeByName(name)
		if !ok {
			return fmt.Errorf("unknown theme %q (themes: %s)", name, strings.Join(themeNames(), ", "))
		}
		st := NewStyle(u.style().modeOr(), t)
		u.Banner(out, "◆ "+t.Name+" — "+t.Description)
		for _, l := range renderPreview(st) {
			fmt.Fprintln(out, l)
		}
		return nil
	}
	return errors.New("usage: vili theme [list | set <name> | preview [name]]")
}

// modeOr returns the style's color mode (ColorOff for nil).
func (s *Style) modeOr() ColorMode {
	if s == nil {
		return ColorOff
	}
	return s.mode
}

// interactive reports whether in and out are both terminals (a picker needs
// to read keys and redraw).
func interactive(in io.Reader, out io.Writer) bool {
	return isTerminal(in) && isTerminal(out)
}

func themeList(out io.Writer, u *UI) error {
	cur := currentTheme(u)
	for _, t := range themes {
		st := NewStyle(u.style().modeOr(), t)
		mark := "  "
		if t.Name == cur {
			mark = u.style().Success("● ")
			if !u.style().Enabled() {
				mark = "* "
			}
		}
		line := fmt.Sprintf("%s%-16s %s", mark, t.Name, u.style().Muted(t.Description))
		if st.Enabled() {
			line += "  " + swatches(st)
		}
		fmt.Fprintln(out, line)
	}
	if !interactive(os.Stdin, out) {
		return nil
	}
	fmt.Fprintln(out, u.style().Muted("\nrun `vili theme` in a terminal for the interactive picker"))
	return nil
}

func saveTheme(out io.Writer, u *UI, name string) error {
	prefs := u.prefs()
	if err := prefs.Set("theme", name); err != nil {
		return err
	}
	if err := prefs.Save(); err != nil {
		return err
	}
	t, _ := themeByName(prefs.Theme)
	nu := *u
	nu.Style = NewStyle(u.style().modeOr(), t)
	nu.Banner(out, "✓ theme set to "+t.Name)
	return nil
}

// prefs returns the saved prefs (nil-safe).
func (u *UI) prefs() Prefs {
	if u == nil {
		return Prefs{}
	}
	return u.Prefs
}

// ---- interactive picker ----------------------------------------------------

// picker is the theme picker's state, kept separate from terminal I/O so it
// can be tested without a TTY.
type picker struct {
	idx       int
	done      bool
	cancelled bool
}

// keys decoded from raw terminal input.
const (
	keyUp     = "up"
	keyDown   = "down"
	keyEnter  = "enter"
	keyCancel = "cancel"
)

// decodeKey maps a raw read from a terminal in raw mode to a key name.
func decodeKey(b []byte) string {
	switch s := string(b); {
	case s == "\x1b[A" || s == "\x1bOA" || s == "k" || s == "K":
		return keyUp
	case s == "\x1b[B" || s == "\x1bOB" || s == "j" || s == "J" || s == "\t":
		return keyDown
	case s == "\r" || s == "\n" || s == " ":
		return keyEnter
	case s == "q" || s == "Q" || s == "\x1b" || s == "\x03" || s == "\x04":
		return keyCancel
	case len(s) == 1 && s[0] >= '1' && s[0] <= '9':
		return s // jump to theme N
	}
	return ""
}

func (p *picker) handle(key string) {
	switch key {
	case keyUp:
		p.idx = (p.idx - 1 + len(themes)) % len(themes)
	case keyDown:
		p.idx = (p.idx + 1) % len(themes)
	case keyEnter:
		p.done = true
	case keyCancel:
		p.done, p.cancelled = true, true
	default:
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			if n := int(key[0] - '1'); n < len(themes) {
				p.idx = n
			}
		}
	}
}

// render draws one frame. phase animates the title gradient. Lines end in
// "\r\n" because the terminal is in raw mode.
func (p *picker) render(mode ColorMode, phase float64) string {
	sel := themes[p.idx]
	st := NewStyle(mode, sel)
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\x1b[K\r\n") }
	line("  " + st.Gradient("✦ vili themes ✦", phase, gradientStops(sel)...) +
		"  " + st.Muted("↑/↓ or j/k move · 1-9 jump · enter apply · q cancel"))
	line("")
	for i, t := range themes {
		tst := NewStyle(mode, t)
		if i == p.idx {
			line(fmt.Sprintf("  %s %s  %s  %s", st.Accent("❯"), tst.Accent(fmt.Sprintf("%-16s", t.Name)), swatches(tst), st.Muted(t.Description)))
		} else {
			line(fmt.Sprintf("    %-16s %s", t.Name, swatches(tst)))
		}
	}
	line("")
	line("  " + st.Muted("preview"))
	for _, l := range renderPreview(st) {
		line("  " + l)
	}
	return b.String()
}

// runThemePicker drives the picker on a real terminal: raw-mode key input,
// in-place redraws, and an animated title.
func runThemePicker(in *os.File, out io.Writer, u *UI) error {
	p := &picker{}
	cur := currentTheme(u)
	for i, t := range themes {
		if t.Name == cur {
			p.idx = i
		}
	}
	mode := u.style().modeOr()
	if mode == ColorOff {
		mode = Color256 // a picker without color shows nothing worth picking
	}

	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return themeList(out, u)
	}
	restore := func() { _ = term.Restore(int(in.Fd()), state) }
	defer restore()
	fmt.Fprint(out, "\x1b[?25l")
	defer fmt.Fprint(out, "\x1b[?25h")

	keys := make(chan string)
	go func() {
		buf := make([]byte, 16)
		for {
			n, err := in.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			keys <- decodeKey(buf[:n])
		}
	}()

	lines := 0
	draw := func(phase float64) {
		if lines > 0 {
			fmt.Fprintf(out, "\x1b[%dA", lines)
		}
		frame := p.render(mode, phase)
		fmt.Fprint(out, "\r"+frame+"\x1b[J")
		lines = strings.Count(frame, "\r\n")
	}
	animate := u != nil && u.Anim
	tick := time.NewTicker(90 * time.Millisecond)
	defer tick.Stop()
	frame := 0
	draw(0)
	for !p.done {
		select {
		case k, ok := <-keys:
			if !ok {
				p.done, p.cancelled = true, true
				break
			}
			p.handle(k)
			draw(float64(frame%50) / 50)
		case <-tick.C:
			if animate {
				frame++
				draw(float64(frame%50) / 50)
			}
		}
	}
	// Clear the picker and leave a one-line result.
	fmt.Fprintf(out, "\x1b[%dA\r\x1b[J", lines)
	restore()
	if p.cancelled {
		fmt.Fprintln(out, "theme unchanged ("+cur+")")
		return nil
	}
	return saveTheme(out, u, themes[p.idx].Name)
}

// ---- config ----------------------------------------------------------------

func cmdConfig(out io.Writer, u *UI, args []string) error {
	prefs := u.prefs()
	st := u.style()
	switch {
	case len(args) == 0:
		for _, kv := range prefs.Entries() {
			fmt.Fprintf(out, "%-16s %s\n", st.Accent(kv[0]), kv[1])
		}
		fmt.Fprintln(out, st.Muted("\nsettable keys (vili config set <key> <value>):"))
		for _, k := range prefKeys {
			fmt.Fprintf(out, "  %-14s %-24s %s\n", k.key, k.values, st.Muted(k.help))
		}
		return nil
	case len(args) == 1 && args[0] == "path":
		p, err := prefsPath()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, p)
		return nil
	case len(args) == 1 && args[0] == "reset":
		if err := (Prefs{}).Save(); err != nil {
			return err
		}
		u.ok(out, "settings reset to defaults")
		return nil
	case len(args) == 3 && args[0] == "set":
		if err := prefs.Set(args[1], args[2]); err != nil {
			return err
		}
		if err := prefs.Save(); err != nil {
			return err
		}
		u.ok(out, "%s = %s", strings.ToLower(args[1]), args[2])
		return nil
	case len(args) == 2 && args[0] == "unset":
		if err := prefs.Unset(args[1]); err != nil {
			return err
		}
		if err := prefs.Save(); err != nil {
			return err
		}
		u.ok(out, "%s reset to default", strings.ToLower(args[1]))
		return nil
	}
	return errors.New("usage: vili config [set <key> <value> | unset <key> | reset | path]")
}
