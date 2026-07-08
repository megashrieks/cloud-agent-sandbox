package session

import "github.com/google/uuid"

// NewID returns a short unique sandbox session ID.
func NewID() string {
	id := uuid.NewString()
	return "sbx-" + id[:8]
}
