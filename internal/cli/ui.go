package cli

import (
	"fmt"
	"io"
	"os"
)

// stderr is where spinners and errors go; a variable so tests can replace it.
var stderr io.Writer = os.Stderr

// UI is the resolved presentation for one invocation: the style for stdout,
// whether animations may run, and display preferences. A nil *UI renders plain
// text with no animations, so every method is nil-safe.
type UI struct {
	Style      *Style // for stdout
	ErrStyle   *Style // for stderr (spinners), which may be a terminal when stdout is not
	Prefs      Prefs
	OutTTY     bool // stdout is a terminal
	ErrTTY     bool // stderr is a terminal
	Anim       bool // animations allowed (pref on, NO_COLOR unset, VILI_NO_ANIM unset)
	Bell       bool
	TimeLayout string
	Err        io.Writer
	Warnings   []string // non-fatal problems with saved settings
}

// uiOverrides are per-invocation flags that win over saved preferences.
type uiOverrides struct {
	Theme  string
	Color  string
	NoAnim bool
}

// newUI resolves the presentation from saved prefs, flags and the environment.
func newUI(out io.Writer, prefs Prefs, o uiOverrides, getenv func(string) string) (*UI, error) {
	if o.Theme == "" {
		o.Theme = getenv("VILI_THEME")
	}
	if o.Color == "" {
		o.Color = getenv("VILI_COLOR")
	}
	// Saved settings that are invalid (e.g. a hand-edited config.json) only
	// warn and fall back, so they can never lock the user out of
	// `vili config reset`; invalid flags and env vars fail loudly.
	var warnings []string
	theme := themes[0]
	if o.Theme != "" {
		t, ok := themeByName(o.Theme)
		if !ok {
			return nil, fmt.Errorf("unknown theme %q (themes: %v)", o.Theme, themeNames())
		}
		theme = t
	} else if prefs.Theme != "" {
		if t, ok := themeByName(prefs.Theme); ok {
			theme = t
		} else {
			warnings = append(warnings, fmt.Sprintf("config: unknown theme %q, using %s", prefs.Theme, theme.Name))
		}
	}
	if over, err := theme.withOverrides(prefs.Colors); err != nil {
		warnings = append(warnings, "config: "+err.Error()+" (color overrides ignored)")
	} else {
		theme = over
	}

	colorPref := prefs.Color
	switch colorPref {
	case "", "auto", "always", "never":
	default:
		warnings = append(warnings, fmt.Sprintf("config: color %q is invalid, using auto", colorPref))
		colorPref = "auto"
	}
	if o.Color != "" {
		switch o.Color {
		case "auto", "always", "never":
			colorPref = o.Color
		default:
			return nil, fmt.Errorf("--color must be auto, always or never (got %q)", o.Color)
		}
	}

	u := &UI{
		Style:    NewStyle(detectColorMode(out, colorPref, getenv), theme),
		ErrStyle: NewStyle(detectColorMode(stderr, colorPref, getenv), theme),
		Prefs:    prefs,
		OutTTY:   isTerminal(out),
		ErrTTY:   isTerminal(stderr),
		Err:      stderr,
		Warnings: warnings,
	}
	u.Anim = (prefs.Animations == nil || *prefs.Animations) && !o.NoAnim &&
		getenv("VILI_NO_ANIM") == "" && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
	u.Bell = prefs.Bell != nil && *prefs.Bell
	u.TimeLayout = "15:04:05"
	if l, ok := timeLayouts[prefs.TimeFormat]; ok {
		u.TimeLayout = l
	}
	return u, nil
}

// style returns the UI's style (nil-safe: a nil style renders plain text).
func (u *UI) style() *Style {
	if u == nil {
		return nil
	}
	return u.Style
}

// opts returns message rendering options for the signed-in user.
func (u *UI) opts(me string) printOpts {
	o := printOpts{Me: me, TimeLayout: "15:04:05"}
	if u != nil {
		o.Style, o.TimeLayout = u.Style, u.TimeLayout
	}
	return o
}

// ok prints a confirmation line, prefixed with a check mark when colored.
func (u *UI) ok(out io.Writer, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if s := u.style(); s.Enabled() {
		msg = s.Success("✓") + " " + msg
	}
	fmt.Fprintln(out, msg)
}

// PrintError writes err to stderr as "vili: <err>", colored when stderr is a
// terminal and the user's preferences allow it.
func PrintError(err error) {
	prefs, _ := LoadPrefs()
	u, uerr := newUI(stderr, prefs, uiOverrides{}, os.Getenv)
	prefix := "vili:"
	if uerr == nil && u.Style.Enabled() {
		prefix = u.Style.Error("✗ vili:")
	}
	fmt.Fprintln(stderr, prefix, err)
}

// bell reports whether to ring on mentions (nil-safe).
func (u *UI) bell() bool { return u != nil && u.Bell }
