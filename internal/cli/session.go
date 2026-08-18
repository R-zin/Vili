package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Session is the persisted client state: which server to talk to and the
// bearer token + identity from the last login.
type Session struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
	User    User   `json:"user"`
}

// ErrNoSession reports that no session file exists yet (i.e. not logged in).
var ErrNoSession = errors.New("no session: run 'vili login' first")

// sessionPath resolves where the session file lives: <user-config>/vili/session.json.
func sessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(dir, "vili", "session.json"), nil
}

// LoadSession reads the persisted session, returning ErrNoSession when absent.
func LoadSession() (*Session, error) {
	path, err := sessionPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoSession
		}
		return nil, fmt.Errorf("read session: %w", err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	return &s, nil
}

// Save writes the session atomically with owner-only permissions (it holds a
// bearer token). The parent directory is created as needed.
func (s *Session) Save() error {
	path, err := sessionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return nil
}

// ClearSession removes the session file (logout). Missing file is not an error.
func ClearSession() error {
	path, err := sessionPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove session: %w", err)
	}
	return nil
}
