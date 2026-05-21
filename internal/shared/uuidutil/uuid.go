package uuidutil

import "github.com/google/uuid"

// NewV7 generates a UUIDv7 for app-owned UUID primary keys.
func NewV7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err)
	}
	return id
}
