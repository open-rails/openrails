// Package booktag is the billing book's logical identity as provider objects
// carry it. OpenRails writes it into every object it creates; a pull reads it
// back to tell an object the database lost from one another book made.
package booktag

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

const (
	// NMIField is the merchant-defined field NMI objects carry the book id in.
	NMIField = "merchant_defined_field_20"
	// NMIFieldID is NMIField's id in query reports and webhooks.
	NMIFieldID = "20"
	// StripeKey is the metadata key Stripe objects carry the book id in.
	StripeKey = "openrails_book"
)

var sources sync.Map // merchant id -> *Source

// Register makes src the book of merchantID's provider writes; the first
// registration stands.
func Register(merchantID uuid.UUID, src *Source) { sources.LoadOrStore(merchantID, src) }

// Registered reports whether merchantID has a book.
func Registered(merchantID uuid.UUID) bool { _, ok := sources.Load(merchantID); return ok }

// For is merchantID's book; nil (which tags nothing) when none is registered.
func For(merchantID uuid.UUID) *Source {
	src, _ := sources.Load(merchantID)
	s, _ := src.(*Source)
	return s
}

// Ensure is merchantID's book, registering one read through q when none is.
func Ensure(merchantID uuid.UUID, q *gen.Queries) *Source {
	Register(merchantID, NewSource(q))
	return For(merchantID)
}

// Tag is the book's identity and when it began tagging.
type Tag struct {
	ID        string
	StartedAt time.Time
}

// Source reads the book's tag from its database once; both values never
// change afterwards. The zero Source and a nil one tag nothing.
type Source struct {
	read func(context.Context) (gen.GetBookTagRow, error)
	mu   sync.Mutex
	tag  *Tag
}

// NewSource reads the tag through q.
func NewSource(q *gen.Queries) *Source {
	return &Source{read: func(ctx context.Context) (gen.GetBookTagRow, error) { return q.GetBookTag(ctx) }}
}

// Tag is the book's tag; false when it cannot be read now.
func (s *Source) Tag(ctx context.Context) (Tag, bool) {
	if s == nil || s.read == nil {
		return Tag{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tag != nil {
		return *s.tag, true
	}
	row, err := s.read(ctx)
	if err != nil {
		return Tag{}, false
	}
	s.tag = &Tag{ID: row.BookID.String(), StartedAt: row.TaggingStartedAt}
	return *s.tag, true
}

// ID is the book id, "" when it cannot be read now.
func (s *Source) ID(ctx context.Context) string {
	tag, _ := s.Tag(ctx)
	return tag.ID
}

// Provenance is whose an unknown provider object is.
type Provenance string

const (
	// Lost objects carry this book's id: the database lost a record of them.
	Lost Provenance = "lost"
	// Foreign objects were made by another book or by hand.
	Foreign Provenance = "foreign"
	// PreTag objects carry no id and predate this book's tagging.
	PreTag Provenance = "pre_tag"
)

// Classify says whose an object is by the book id it carries (any case,
// surrounding space ignored; "" for none) and when it was created (zero when
// unknown, which is never taken as after tagging).
func (t Tag) Classify(carried string, createdAt time.Time) Provenance {
	carried = strings.ToLower(strings.TrimSpace(carried))
	switch {
	case carried != "" && carried == strings.ToLower(t.ID):
		return Lost
	case carried != "":
		return Foreign
	case !createdAt.IsZero() && !t.StartedAt.IsZero() && !createdAt.Before(t.StartedAt):
		return Foreign
	}
	return PreTag
}

// Quiet reports whether an unknown, unmatched object of this provenance is
// informational on a PSP that moves real money or not.
func (p Provenance) Quiet(live bool) bool {
	return p == Foreign || (p == PreTag && !live)
}
