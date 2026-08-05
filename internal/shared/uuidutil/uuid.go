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

// DeterministicNamespace is the PERMANENT uuidv5 namespace for natural-key
// derived ids (#662). It must NEVER change: the derived id is a pure function
// of (this namespace, the natural key), so changing it re-mints every product,
// price, and PSP id and orphans every FK that references one.
var DeterministicNamespace = uuid.MustParse("6f2a1bc3-51cd-4daa-844f-99d170240561")

// DeterministicID derives a stable uuidv5 from an entity's immutable natural
// key (#662): the same parts always yield the same id — in every process and
// every database — so a logical entity keeps one identity across environments
// and fresh rebuilds, computable without a DB read.
//
// parts are joined with a length-prefixed, injective encoding: each part is
// preceded by its 8-byte big-endian length. This makes the encoding
// unambiguous across field boundaries — no separator can collide with a part's
// contents (e.g. a CCBill account_id contains '-', a natural key may contain
// any byte), and ("a","bc") never encodes the same as ("ab","c"). Callers pass
// DeterministicNamespace; a differing part count or content yields a different
// id, so structurally different natural keys (2-part product vs 3-part provider
// vs 7-part price) cannot collide.
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
