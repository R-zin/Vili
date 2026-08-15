package message
import (
	"time"
	"github.com/google/uuid")

type MessageType string

const (
	TypeText MessageType = "text"
	TypeDiff MessageType = "diff"
	TypeCode MessageType = "code"
	TypeLog MessageType = "log"
	TypeCommit MessageType = "commit"
)

type Message struct {
	ID		uuid.UUID
	RoomID		uuid.UUID
	UserID		uuid.UUID
	Username	string
	Content		string
	Type MessageType
	CreatedAt	time.Time
}