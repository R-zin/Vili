package bus

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestLocalDeliversToAllSubscribers(t *testing.T) {
	b := NewLocal()
	var a, c []Message
	b.Subscribe(func(m Message) { a = append(a, m) })
	b.Subscribe(func(m Message) { c = append(c, m) })
	m := Message{Kind: KindDisconnect, Room: uuid.New(), User: uuid.New()}
	if err := b.Publish(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || len(c) != 1 || a[0] != m {
		t.Fatalf("delivery: %v %v", a, c)
	}
}

func TestNewInstanceID(t *testing.T) {
	a, b := NewInstanceID("web-1"), NewInstanceID("")
	if a == b || len(a) < 10 || b[:5] != "vili-" {
		t.Fatalf("ids: %q %q", a, b)
	}
}
