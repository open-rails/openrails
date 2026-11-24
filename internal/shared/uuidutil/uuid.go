package uuidutil

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

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

// DerivedV7 is the UUIDv7 an idempotency key names: its timestamp is at (in
// milliseconds) and its other 74 bits hash SHA-256(namespace || name), so the
// same inputs give the same id. at is an instant the inputs fix: the event
// time the name carries, else the parent's TimeOf; never the clock. An instant
// before the Unix epoch counts as the epoch.
func DerivedV7(at time.Time, namespace uuid.UUID, name string) uuid.UUID {
	h := sha256.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	var u uuid.UUID
	copy(u[6:], h.Sum(nil)[:10])
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(max(at.UnixMilli(), 0))<<16) // #nosec G115 -- not negative
	copy(u[:6], ts[:6])
	u[6] = 0x70 | u[6]&0x0f
	u[8] = 0x80 | u[8]&0x3f
	return u
}

// ChildV7 is the id parent derives for name: DerivedV7 at TimeOf(parent) in
// parent's namespace.
func ChildV7(parent uuid.UUID, name string) uuid.UUID {
	return DerivedV7(TimeOf(parent), parent, name)
}

// TimeOf is the instant a UUIDv7 carries. Any other version (an id minted
// before ids were v7, or one a provider chose) reads as the Unix epoch, so
// what derives from it stays deterministic and sorts first.
func TimeOf(id uuid.UUID) time.Time {
	if id.Version() != 7 {
		return time.UnixMilli(0).UTC()
	}
	var ts [8]byte
	copy(ts[2:], id[:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(ts[:]))).UTC() // #nosec G115 -- 48 bits
}

// DeterministicNamespace is the permanent namespace for natural-key ids.
// Changing it re-mints every derived id and orphans every FK to one.
var DeterministicNamespace = uuid.MustParse("6f2a1bc3-51cd-4daa-844f-99d170240561")

// DeterministicID derives a stable uuidv5 from the immutable natural key of
// an object with no time of its own (a declared PSP, a catalog product or
// price): the same parts yield the same id in every process and database.
func DeterministicID(namespace uuid.UUID, parts ...string) uuid.UUID {
	return uuid.NewSHA1(namespace, []byte(Name(parts...)))
}

// Name encodes parts injectively, each prefixed with its 8-byte big-endian
// length: ("a","bc") never matches ("ab","c").
func Name(parts ...string) string {
	var buf []byte
	var lp [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(lp[:], uint64(len(p)))
		buf = append(buf, lp[:]...)
		buf = append(buf, p...)
	}
	return string(buf)
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
