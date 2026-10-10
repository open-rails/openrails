package uuidutil

import (
	"encoding/binary"

	"github.com/google/uuid"
)

// NewV7 generates a UUIDv7 for app-owned UUID primary keys.
func NewV7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err)
	}
	return id
}

// DeterministicNamespace is the permanent uuidv5 namespace for natural-key ids.
// Changing it re-mints every derived id and orphans every FK to one.
var DeterministicNamespace = uuid.MustParse("6f2a1bc3-51cd-4daa-844f-99d170240561")

// DeterministicID derives a stable uuidv5 from an entity's immutable natural
// key: the same parts yield the same id in every process and database. Each
// part is prefixed with its 8-byte big-endian length, so the encoding is
// injective: ("a","bc") never matches ("ab","c").
func DeterministicID(namespace uuid.UUID, parts ...string) uuid.UUID {
	var buf []byte
	var lp [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(lp[:], uint64(len(p)))
		buf = append(buf, lp[:]...)
		buf = append(buf, p...)
	}
	return uuid.NewSHA1(namespace, buf)
}

// Of is typed ids as their UUIDs; nil stays nil.
func Of[T interface{ UUID() uuid.UUID }](ids []T) []uuid.UUID {
	if ids == nil {
		return nil
	}
	out := make([]uuid.UUID, len(ids))
	for i, id := range ids {
		out[i] = id.UUID()
	}
	return out
}
