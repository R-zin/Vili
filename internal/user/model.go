package user
import (
	"time"
	"github.com/google/uuid"
)
type User struct {
	ID       uuid.UUID
	Username string 
	PasswordHash string
	CreatedAt time.Time
}
