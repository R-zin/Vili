package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Session is the persisted client state: which server to talk to, the bearer
// token + its expiry, the refresh token used to renew it, and the identity from
// the last login. RefreshToken and TokenExpiry are zero for sessions saved
// before refresh tokens existed; such sessions simply require a fresh login
// once the access token expires.
type Session struct {
	BaseURL      string    `json:"base_url"`
	Token        string    `json:"token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenExpiry  time.Time `json:"token_expiry,omitempty"`
	User         User      `json:"user"`
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}

	// Atomic write: create a temporary file in the same directory, sync, chmod, and rename.
	tmpFile, err := os.CreateTemp(dir, "session-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp session: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod temp session: %w", err)
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp session: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync temp session: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp session: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	_ = os.Chmod(path, 0o600)
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
