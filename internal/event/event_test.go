package event

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestNewMessageRoundTrip(t *testing.T) {
	roomID := uuid.New()
	payload := map[string]any{"content": "hello", "username": "alice", "type": "text"}
	e, err := NewMessage(roomID, payload)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if e.Type != MessageNew {
		t.Errorf("type = %q, want %q", e.Type, MessageNew)
	}
	if e.RoomID != roomID {
		t.Errorf("room = %v, want %v", e.RoomID, roomID)
	}
	var got map[string]any
	if err := json.Unmarshal(e.Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got["username"] != "alice" {
		t.Errorf("payload username = %v, want alice", got["username"])
	}
}

func TestPresenceStateRoundTrip(t *testing.T) {
	roomID := uuid.New()
	e := NewPresenceState(roomID, []string{"alice", "bob"})
	if e.Type != PresenceState {
		t.Fatalf("type = %q, want %q", e.Type, PresenceState)
	}
	var p struct {
		Online []string `json:"online"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Online) != 2 || p.Online[0] != "alice" {
		t.Errorf("online = %v, want [alice bob]", p.Online)
	}
}

func TestTypingUsername(t *testing.T) {
	roomID := uuid.New()
	name, err := TypingUsername(NewTyping(roomID, "alice"))
	if err != nil {
		t.Fatalf("TypingUsername: %v", err)
	}
	if name != "alice" {
		t.Errorf("username = %q, want alice", name)
	}

	if _, err := TypingUsername(NewPresenceJoin(roomID, "alice")); err == nil {
		t.Error("expected error extracting typing username from a non-typing event")
	}
}
