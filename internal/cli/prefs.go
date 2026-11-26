package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Prefs are the user's terminal customizations, persisted next to the session
// in <user-config>/vili/config.json. Zero values mean "default".
type Prefs struct {
	Theme      string            `json:"theme,omitempty"`
	Color      string            `json:"color,omitempty"`       // auto | always | never
	Animations *bool             `json:"animations,omitempty"`  // default on
	Bell       *bool             `json:"bell,omitempty"`        // ring on @mention; default off
	TimeFormat string            `json:"time_format,omitempty"` // full | short | 12h | off
	Colors     map[string]string `json:"colors,omitempty"`      // per-role overrides
}

// timeLayouts maps time-format presets to Go layouts ("" hides timestamps).
var timeLayouts = map[string]string{
	"full":  "15:04:05",
	"short": "15:04",
	"12h":   "3:04 PM",
	"off":   "",
}

// prefKeys documents the settable keys for `vili config`.
var prefKeys = []struct{ key, values, help string }{
	{"theme", strings.Join(themeNames(), "|"), "color theme"},
	{"color", "auto|always|never", "when to use colors (auto = only on a terminal)"},
	{"animations", "on|off", "spinners and animated banners"},
	{"bell", "on|off", "ring the terminal bell when you are @mentioned in chat"},
	{"time-format", "full|short|12h|off", "message timestamps"},
	{"color.<role>", "#rrggbb", "override one theme color; roles: " + strings.Join(themeRoles, ", ")},
}

func prefsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "vili", "config.json"), nil
}

// LoadPrefs reads the saved preferences; a missing file yields defaults.
func LoadPrefs() (Prefs, error) {
	path, err := prefsPath()
	if err != nil {
		return Prefs{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Prefs{}, nil
		}
		return Prefs{}, fmt.Errorf("read config: %w", err)
	}
	var p Prefs
	if err := json.Unmarshal(data, &p); err != nil {
		return Prefs{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return p, nil
}

// Save writes the preferences atomically.
func (p Prefs) Save() error {
	path, err := prefsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

func parseOnOff(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q must be on or off", v)
}

// Set validates and applies one key.
func (p *Prefs) Set(key, value string) error {
	key = strings.ToLower(key)
	switch {
	case key == "theme":
		t, ok := themeByName(value)
		if !ok {
			return fmt.Errorf("unknown theme %q (themes: %s)", value, strings.Join(themeNames(), ", "))
		}
		p.Theme = t.Name
	case key == "color":
		if value != "auto" && value != "always" && value != "never" {
			return errors.New("color must be auto, always or never")
		}
		p.Color = value
	case key == "animations" || key == "bell":
		b, err := parseOnOff(value)
		if err != nil {
			return err
		}
		if key == "bell" {
			p.Bell = &b
		} else {
			p.Animations = &b
		}
	case key == "time-format":
		if _, ok := timeLayouts[value]; !ok {
			return errors.New("time-format must be full, short, 12h or off")
		}
		p.TimeFormat = value
	case strings.HasPrefix(key, "color."):
		role := strings.TrimPrefix(key, "color.")
		var t Theme
		if t.role(role) == nil {
			return fmt.Errorf("unknown color role %q (roles: %s)", role, strings.Join(themeRoles, ", "))
		}
		if _, _, _, err := parseHex(value); err != nil {
			return err
		}
		if p.Colors == nil {
			p.Colors = map[string]string{}
		}
		p.Colors[role] = strings.ToLower(value)
	default:
		return fmt.Errorf("unknown setting %q — see: vili config", key)
	}
	return nil
}

// Unset restores one key to its default.
func (p *Prefs) Unset(key string) error {
	key = strings.ToLower(key)
	switch {
	case key == "theme":
		p.Theme = ""
	case key == "color":
		p.Color = ""
	case key == "animations":
		p.Animations = nil
	case key == "bell":
		p.Bell = nil
	case key == "time-format":
		p.TimeFormat = ""
	case key == "colors":
		p.Colors = nil
	case strings.HasPrefix(key, "color."):
		delete(p.Colors, strings.TrimPrefix(key, "color."))
		if len(p.Colors) == 0 {
			p.Colors = nil
		}
	default:
		return fmt.Errorf("unknown setting %q — see: vili config", key)
	}
	return nil
}

// Entries returns the effective settings as sorted key/value pairs.
func (p Prefs) Entries() [][2]string {
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	onOff := func(b *bool, def bool) string {
		v := def
		if b != nil {
			v = *b
		}
		if v {
			return "on"
		}
		return "off"
	}
	out := [][2]string{
		{"theme", or(p.Theme, themes[0].Name)},
		{"color", or(p.Color, "auto")},
		{"animations", onOff(p.Animations, true)},
		{"bell", onOff(p.Bell, false)},
		{"time-format", or(p.TimeFormat, "full")},
	}
	roles := make([]string, 0, len(p.Colors))
	for r := range p.Colors {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		out = append(out, [2]string{"color." + r, p.Colors[r]})
	}
	return out
}
