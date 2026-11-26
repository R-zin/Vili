package cli

import (
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ColorMode is how much color the terminal can show.
type ColorMode int

// Supported color modes, from none to 24-bit.
const (
	ColorOff ColorMode = iota
	Color256
	ColorTrue
)

// Theme is a named palette. Every color is a "#rrggbb" hex string; an empty
// string means "no color for this role" (the role still gets its weight, e.g.
// bold usernames), which is how the mono theme works.
type Theme struct {
	Name        string
	Description string
	Accent      string // headings, room names, highlights
	Muted       string // ids, hints, secondary text
	Time        string // timestamps
	System      string // presence/typing/system lines
	Success     string
	Warn        string // unread counters
	Error       string
	Mention     string // messages and names that @mention you
	Code        string // code/commit payloads
	Add         string // diff additions
	Del         string // diff removals
	Users       []string
}

// themeRoles lists the overridable color roles (config keys are "color.<role>").
var themeRoles = []string{"accent", "muted", "time", "system", "success", "warn", "error", "mention", "code", "add", "del"}

// themes are the built-in palettes. The first is the default.
var themes = []Theme{
	{
		Name: "classic", Description: "Vili's default: calm blues with warm highlights",
		Accent: "#5fafff", Muted: "#8a8a8a", Time: "#6c6c6c", System: "#87afaf", Success: "#5fd75f",
		Warn: "#ffaf00", Error: "#ff5f5f", Mention: "#ffd75f", Code: "#d7af87", Add: "#5fd75f", Del: "#ff5f5f",
		Users: []string{"#ff8787", "#ffaf5f", "#d7d75f", "#87d787", "#5fd7d7", "#87afff", "#af87ff", "#ff87d7"},
	},
	{
		Name: "dracula", Description: "dark purple with neon accents",
		Accent: "#bd93f9", Muted: "#6272a4", Time: "#6272a4", System: "#8be9fd", Success: "#50fa7b",
		Warn: "#ffb86c", Error: "#ff5555", Mention: "#f1fa8c", Code: "#ff79c6", Add: "#50fa7b", Del: "#ff5555",
		Users: []string{"#ff79c6", "#bd93f9", "#8be9fd", "#50fa7b", "#ffb86c", "#f1fa8c", "#ff5555"},
	},
	{
		Name: "nord", Description: "arctic, north-bluish calm",
		Accent: "#88c0d0", Muted: "#7b88a1", Time: "#7b88a1", System: "#81a1c1", Success: "#a3be8c",
		Warn: "#ebcb8b", Error: "#bf616a", Mention: "#ebcb8b", Code: "#b48ead", Add: "#a3be8c", Del: "#bf616a",
		Users: []string{"#8fbcbb", "#88c0d0", "#81a1c1", "#5e81ac", "#b48ead", "#a3be8c", "#d08770", "#ebcb8b"},
	},
	{
		Name: "gruvbox", Description: "retro groove, warm and earthy",
		Accent: "#fabd2f", Muted: "#928374", Time: "#928374", System: "#83a598", Success: "#b8bb26",
		Warn: "#fe8019", Error: "#fb4934", Mention: "#fabd2f", Code: "#d3869b", Add: "#b8bb26", Del: "#fb4934",
		Users: []string{"#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c", "#fe8019"},
	},
	{
		Name: "solarized-dark", Description: "Solarized for dark backgrounds",
		Accent: "#268bd2", Muted: "#657b83", Time: "#657b83", System: "#2aa198", Success: "#859900",
		Warn: "#b58900", Error: "#dc322f", Mention: "#b58900", Code: "#6c71c4", Add: "#859900", Del: "#dc322f",
		Users: []string{"#b58900", "#cb4b16", "#dc322f", "#d33682", "#6c71c4", "#268bd2", "#2aa198", "#859900"},
	},
	{
		Name: "solarized-light", Description: "Solarized for light backgrounds",
		Accent: "#268bd2", Muted: "#93a1a1", Time: "#93a1a1", System: "#2aa198", Success: "#859900",
		Warn: "#cb4b16", Error: "#dc322f", Mention: "#d33682", Code: "#6c71c4", Add: "#859900", Del: "#dc322f",
		Users: []string{"#b58900", "#cb4b16", "#dc322f", "#d33682", "#6c71c4", "#268bd2", "#2aa198", "#859900"},
	},
	{
		Name: "catppuccin", Description: "soothing pastels (mocha)",
		Accent: "#cba6f7", Muted: "#7f849c", Time: "#7f849c", System: "#89dceb", Success: "#a6e3a1",
		Warn: "#fab387", Error: "#f38ba8", Mention: "#f9e2af", Code: "#f5c2e7", Add: "#a6e3a1", Del: "#f38ba8",
		Users: []string{"#f5e0dc", "#f5c2e7", "#cba6f7", "#f38ba8", "#fab387", "#a6e3a1", "#94e2d5", "#89b4fa"},
	},
	{
		Name: "matrix", Description: "green phosphor, follow the white rabbit",
		Accent: "#00ff41", Muted: "#008f11", Time: "#008f11", System: "#00b82e", Success: "#00ff41",
		Warn: "#a6ff00", Error: "#ff3b3b", Mention: "#d4ff00", Code: "#39ff14", Add: "#00ff41", Del: "#ff3b3b",
		Users: []string{"#00ff41", "#00d936", "#39ff14", "#7dff6a", "#00b82e"},
	},
	{
		Name: "mono", Description: "no colors, just weight and emphasis",
	},
}

// themeByName finds a built-in theme (case-insensitive).
func themeByName(name string) (Theme, bool) {
	for _, t := range themes {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return Theme{}, false
}

// themeNames returns the built-in theme names in display order.
func themeNames() []string {
	out := make([]string, len(themes))
	for i, t := range themes {
		out[i] = t.Name
	}
	return out
}

// withOverrides returns t with per-role colors replaced ("accent" → "#ff0000").
func (t Theme) withOverrides(over map[string]string) (Theme, error) {
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, role := range keys {
		hex := over[role]
		if _, _, _, err := parseHex(hex); err != nil {
			return t, fmt.Errorf("color.%s: %w", role, err)
		}
		p := t.role(role)
		if p == nil {
			return t, fmt.Errorf("unknown color role %q (roles: %s)", role, strings.Join(themeRoles, ", "))
		}
		*p = hex
	}
	return t, nil
}

// role returns a pointer to the theme field for a role name, or nil.
func (t *Theme) role(name string) *string {
	switch strings.ToLower(name) {
	case "accent":
		return &t.Accent
	case "muted":
		return &t.Muted
	case "time":
		return &t.Time
	case "system":
		return &t.System
	case "success":
		return &t.Success
	case "warn":
		return &t.Warn
	case "error":
		return &t.Error
	case "mention":
		return &t.Mention
	case "code":
		return &t.Code
	case "add":
		return &t.Add
	case "del":
		return &t.Del
	}
	return nil
}

// parseHex parses "#rgb" or "#rrggbb".
func parseHex(s string) (r, g, b uint8, err error) {
	h := strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return 0, 0, 0, fmt.Errorf("%q is not a #rrggbb color", s)
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%q is not a #rrggbb color", s)
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v), nil
}

// Style renders text with a theme in a given color mode. A nil *Style, or one
// in ColorOff mode, returns text unchanged, so call sites never need to check.
type Style struct {
	mode  ColorMode
	theme Theme
}

// NewStyle builds a Style.
func NewStyle(mode ColorMode, t Theme) *Style { return &Style{mode: mode, theme: t} }

// Enabled reports whether this style emits escape codes.
func (s *Style) Enabled() bool { return s != nil && s.mode != ColorOff }

// Theme returns the active theme (the default when s is nil).
func (s *Style) Theme() Theme {
	if s == nil {
		return themes[0]
	}
	return s.theme
}

const reset = "\x1b[0m"

// sgrFG returns the escape sequence selecting hex as the foreground color.
func (s *Style) sgrFG(hex string) string {
	if hex == "" {
		return ""
	}
	r, g, b, err := parseHex(hex)
	if err != nil {
		return ""
	}
	if s.mode == ColorTrue {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", to256(r, g, b))
}

// paint wraps text in the given SGR attributes and foreground color.
func (s *Style) paint(text, hex string, attrs ...string) string {
	if !s.Enabled() || text == "" {
		return text
	}
	pre := s.sgrFG(hex)
	for _, a := range attrs {
		pre += "\x1b[" + a + "m"
	}
	if pre == "" {
		return text
	}
	return pre + text + reset
}

// Role helpers.
func (s *Style) Accent(t string) string  { return s.paint(t, s.Theme().Accent, "1") }
func (s *Style) Muted(t string) string   { return s.paint(t, s.Theme().Muted, s.dimIfMono()) }
func (s *Style) Time(t string) string    { return s.paint(t, s.Theme().Time, s.dimIfMono()) }
func (s *Style) System(t string) string  { return s.paint(t, s.Theme().System, "3") }
func (s *Style) Success(t string) string { return s.paint(t, s.Theme().Success, "1") }
func (s *Style) Warn(t string) string    { return s.paint(t, s.Theme().Warn, "1") }
func (s *Style) Error(t string) string   { return s.paint(t, s.Theme().Error, "1") }
func (s *Style) Mention(t string) string { return s.paint(t, s.Theme().Mention, "1") }
func (s *Style) Code(t string) string    { return s.paint(t, s.Theme().Code) }
func (s *Style) Add(t string) string     { return s.paint(t, s.Theme().Add) }
func (s *Style) Del(t string) string     { return s.paint(t, s.Theme().Del) }
func (s *Style) Bold(t string) string    { return s.paint(t, "", "1") }
func (s *Style) Italic(t string) string  { return s.paint(t, "", "3") }

// dimIfMono uses the faint attribute where the theme has no colors, so muted
// text still reads as secondary.
func (s *Style) dimIfMono() string {
	if s != nil && s.theme.Muted == "" {
		return "2"
	}
	return ""
}

// User colors a username with a stable per-name color from the theme's palette,
// so the same person always has the same color.
func (s *Style) User(name string) string {
	users := s.Theme().Users
	if len(users) == 0 {
		return s.paint(name, "", "1")
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return s.paint(name, users[h.Sum32()%uint32(len(users))], "1")
}

// Gradient colors each rune of text along a gradient that cycles through the
// given hex stops. phase (0..1) shifts the gradient, which animations advance
// frame by frame to make it sweep.
func (s *Style) Gradient(text string, phase float64, stops ...string) string {
	if !s.Enabled() || len(stops) == 0 || stops[0] == "" {
		return s.Bold(text)
	}
	runes := []rune(text)
	var b strings.Builder
	for i, r := range runes {
		if r == ' ' {
			b.WriteRune(r)
			continue
		}
		pos := float64(i)/float64(max(len(runes), 1)) + phase
		pos -= float64(int(pos)) // wrap into [0,1)
		b.WriteString(s.sgrFG(lerpStops(stops, pos)))
		b.WriteString("\x1b[1m")
		b.WriteRune(r)
	}
	b.WriteString(reset)
	return b.String()
}

// lerpStops interpolates a cyclic gradient through stops at t in [0,1).
func lerpStops(stops []string, t float64) string {
	if len(stops) == 1 {
		return stops[0]
	}
	seg := t * float64(len(stops))
	i := int(seg) % len(stops)
	j := (i + 1) % len(stops)
	f := seg - float64(int(seg))
	r1, g1, b1, _ := parseHex(stops[i])
	r2, g2, b2, _ := parseHex(stops[j])
	mix := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*f + 0.5) }
	return fmt.Sprintf("#%02x%02x%02x", mix(r1, r2), mix(g1, g2), mix(b1, b2))
}

// to256 maps an RGB color to the nearest xterm-256 palette index (6x6x6 cube
// or grayscale ramp).
func to256(r, g, b uint8) int {
	if r == g && g == b {
		if r < 8 {
			return 16
		}
		if r > 248 {
			return 231
		}
		return 232 + int((float64(r)-8)/247*24+0.5)
	}
	q := func(v uint8) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return (int(v) - 35) / 40
	}
	return 16 + 36*q(r) + 6*q(g) + q(b)
}

// isTerminal reports whether w is a terminal.
func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// detectColorMode resolves the effective color mode for w from the user's
// preference ("auto", "always", "never") and the environment. NO_COLOR
// (https://no-color.org) disables color unless "always" is explicit; "auto"
// enables color only on a real terminal that is not TERM=dumb.
func detectColorMode(w io.Writer, pref string, getenv func(string) string) ColorMode {
	depth := Color256
	if ct := strings.ToLower(getenv("COLORTERM")); ct == "truecolor" || ct == "24bit" {
		depth = ColorTrue
	}
	switch pref {
	case "never":
		return ColorOff
	case "always":
		return depth
	}
	if getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" || !isTerminal(w) {
		return ColorOff
	}
	return depth
}
