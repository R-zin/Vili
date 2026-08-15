package model

import (
	"time"	
	"github.com/google/uuid"
)
type Room struct {
	ID       uuid.UUID
	Name     string 
	Description string
	CreatedAt time.Time
	CreatedBy uuid.UUID
	UpdatedAt time.Time
}

type MemberRole string

const (
	RoleOwner MemberRole = "owner"
	RoleAdmin MemberRole = "admin"
	RoleMember MemberRole = "member"
)

type RoomMember struct {
	RoomID   uuid.UUID
	UserID   uuid.UUID
	Role     MemberRole
	JoinedAt time.Time
}
