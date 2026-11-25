// Package controls is each merchant's operator controls, one row of
// billing.merchant_controls: how much OpenRails writes to its providers, the
// rebill brake on the charges OpenRails initiates for it, and whether its
// destructive convergence runs. A merchant with no row writes fully, rebills
// and stays disarmed. A database the billing book was never armed in writes
// nothing: two live copies of one book would bill its customers twice.
package controls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/history"
	"github.com/open-rails/openrails/internal/merchant"
)

// Mode is ordered: ReadOnly < Limited < Full.
type Mode int8

const (
	// ReadOnly: no provider write, reactive or proactive.
	ReadOnly Mode = iota
	// Limited: reactive writes a person asked for; proactive ones wait.
	Limited
	// Full: every provider write.
	Full
)

// ParseMode reads full, limited or readonly.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "full":
		return Full, true
	case "limited":
		return Limited, true
	case "readonly":
		return ReadOnly, true
	}
	return ReadOnly, false
}

func (m Mode) String() string {
	switch m {
	case Full:
		return "full"
	case Limited:
		return "limited"
	}
	return "readonly"
}

// Posture is one merchant's effective provider writes.
type Posture struct {
	Mode Mode
	// Reason says why writes wait; empty at Full.
	Reason string
}

// ReadOnly: every provider write waits.
func (p Posture) ReadOnly() bool { return p.Mode == ReadOnly }

// Limited: proactive (system-origin) provider writes wait.
func (p Posture) Limited() bool { return p.Mode < Full }

// Reader is merchants' effective postures. View is one; Fixed is one for a
// wire with no database behind it.
type Reader interface {
	Posture(ctx context.Context, merchantID uuid.UUID) Posture
}

// Fixed is every merchant at one mode.
type Fixed Mode

// Posture is the fixed mode.
func (f Fixed) Posture(context.Context, uuid.UUID) Posture {
	if Mode(f) == Full {
		return Posture{Mode: Full}
	}
	return Posture{Mode(f), "writes are fixed at " + Mode(f).String()}
}

// Controls is one merchant's controls as stored.
type Controls struct {
	Writes           Mode
	RebillsHeld      bool
	DestructiveArmed bool
	// FirstPullCompletedAt is when a completed pull first surveyed the
	// merchant; nil before.
	FirstPullCompletedAt *time.Time
	// ChangedBy, Reason and ChangedAt are the last change; empty before one.
	ChangedBy string
	Reason    string
	ChangedAt time.Time
	// BookArmed: this database is the billing book's live copy.
	BookArmed bool
}

// Posture is the controls' effective writes: readonly in a copy of the book.
func (c Controls) Posture() Posture {
	if !c.BookArmed {
		return Posture{ReadOnly, "this database is a copy of the billing book: run `openrails book arm` once it is the only live copy"}
	}
	if c.Writes == Full {
		return Posture{Mode: Full}
	}
	reason := "the merchant's writes are " + c.Writes.String()
	if c.Reason != "" {
		reason += " (" + c.Reason + ")"
	}
	return Posture{c.Writes, reason + ": `openrails merchant writes full` restores them"}
}

// Load reads merchantID's controls.
func Load(ctx context.Context, q *gen.Queries, merchantID uuid.UUID) (Controls, error) {
	row, err := q.GetMerchantControls(ctx, merchantID)
	if err != nil {
		return Controls{}, fmt.Errorf("read merchant controls: %w", err)
	}
	writes, ok := ParseMode(row.Writes)
	if !ok {
		return Controls{}, fmt.Errorf("unknown stored writes %q", row.Writes)
	}
	c := Controls{Writes: writes, RebillsHeld: row.RebillsHeld, DestructiveArmed: row.DestructiveArmed, FirstPullCompletedAt: row.FirstPullCompletedAt, BookArmed: row.BookArmed}
	if row.ChangedBy != nil && row.Reason != nil && row.ChangedAt != nil {
		c.ChangedBy, c.Reason, c.ChangedAt = *row.ChangedBy, *row.Reason, row.ChangedAt.UTC()
	}
	return c, nil
}

// View reads merchants' controls from the database. It fails closed: without
// a database, or when the controls cannot be read, writes are readonly and
// rebills held.
type View struct {
	DB *db.DB
}

// Get is merchantID's controls; unreadable controls are an error.
func (v View) Get(ctx context.Context, merchantID uuid.UUID) (Controls, error) {
	if v.DB == nil {
		return Controls{}, errors.New("merchant controls unreadable (no database wired)")
	}
	return Load(ctx, v.DB.Gen(ctx), merchantID)
}

// Posture is merchantID's effective writes.
func (v View) Posture(ctx context.Context, merchantID uuid.UUID) Posture {
	c, err := v.Get(ctx, merchantID)
	if err != nil {
		return Posture{ReadOnly, err.Error()}
	}
	return c.Posture()
}

// Read is merchantID's effective writes; unreadable controls are an error to
// retry, not a posture.
func (v View) Read(ctx context.Context, merchantID uuid.UUID) (Posture, error) {
	c, err := v.Get(ctx, merchantID)
	if err != nil {
		return Posture{}, err
	}
	return c.Posture(), nil
}

// ErrRebillsHeld refuses a merchant-initiated charge of a merchant whose
// rebills are held: it waits for their release.
var ErrRebillsHeld = errors.New("the merchant's rebills are held")

// RebillHold is why merchantID's merchant-initiated charges wait: empty when
// they may run. A customer-initiated charge never asks.
func (v View) RebillHold(ctx context.Context, merchantID uuid.UUID) string {
	c, err := v.Get(ctx, merchantID)
	switch {
	case err != nil:
		return "rebills wait: " + err.Error()
	case c.RebillsHeld:
		return RebillsHeldReason(c)
	}
	return ""
}

// RebillsHeldReason says why held rebills wait.
func RebillsHeldReason(c Controls) string {
	reason := "the merchant's rebills are held"
	if c.Reason != "" {
		reason += " (" + c.Reason + ")"
	}
	return reason + ": `openrails merchant rebills release` resumes them"
}

// Change is what a change of controls sets; a nil field stays as it is.
type Change struct {
	Writes           *Mode
	RebillsHeld      *bool
	DestructiveArmed *bool
}

// Set applies change to merchantID's controls in q's transaction and records
// it in the merchant's configuration history. changed is false when every
// control already had its value: nothing is written then.
func Set(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, change Change, by, reason string, at time.Time) (after Controls, changed bool, err error) {
	by, reason = strings.TrimSpace(by), strings.TrimSpace(reason)
	if by == "" || reason == "" {
		return Controls{}, false, errors.New("a change of merchant controls needs who made it and why")
	}
	if err := q.LockMerchantControls(ctx, merchantID); err != nil {
		return Controls{}, false, err
	}
	before, err := Load(ctx, q, merchantID)
	if err != nil {
		return Controls{}, false, err
	}
	after = before
	fields := map[string]history.FieldChange{}
	if change.Writes != nil && *change.Writes != before.Writes {
		after.Writes = *change.Writes
		fields["writes"] = field(before.Writes.String(), after.Writes.String())
	}
	if change.RebillsHeld != nil && *change.RebillsHeld != before.RebillsHeld {
		after.RebillsHeld = *change.RebillsHeld
		fields["rebills_held"] = field(before.RebillsHeld, after.RebillsHeld)
	}
	if change.DestructiveArmed != nil && *change.DestructiveArmed != before.DestructiveArmed {
		after.DestructiveArmed = *change.DestructiveArmed
		fields["destructive_armed"] = field(before.DestructiveArmed, after.DestructiveArmed)
	}
	if len(fields) == 0 {
		return before, false, nil
	}
	after.ChangedBy, after.Reason, after.ChangedAt = by, reason, at.UTC().Truncate(time.Microsecond)
	fields["changed_by"] = field(before.ChangedBy, by)
	if before.ChangedBy == "" {
		fields["changed_by"] = history.FieldChange{After: mustJSON(by)}
	}
	if err := q.SetMerchantControls(ctx, gen.SetMerchantControlsParams{
		MerchantID: merchantID, Writes: after.Writes.String(), RebillsHeld: after.RebillsHeld, DestructiveArmed: after.DestructiveArmed,
		ChangedBy: by, Reason: reason, ChangedAt: after.ChangedAt,
	}); err != nil {
		return Controls{}, false, err
	}
	provenance := history.ProvenanceAt(ctx, after.ChangedAt)
	provenance.Reason = reason
	if err := history.Record(ctx, q, history.ChangeLog, merchantID, provenance, history.Entry{
		ObjectType: billing.LogObjectControls, ObjectID: merchantID.String(), Action: billing.LogActionUpdate,
		Changes: history.Fields{Version: history.Version, Fields: fields},
	}); err != nil {
		return Controls{}, false, err
	}
	return after, true, nil
}

// Apply is Set in a transaction of its own, in merchantID's scope.
func Apply(ctx context.Context, database *db.DB, merchantID billing.MerchantID, change Change, by, reason string, at time.Time) (after Controls, changed bool, err error) {
	err = database.MerchantTx(merchant.WithID(ctx, merchantID), func(ctx context.Context, tx pgx.Tx) error {
		after, changed, err = Set(ctx, database.NewWithPgxTx(tx).Gen(ctx), merchantID.UUID(), change, by, reason, at)
		return err
	})
	return after, changed, err
}

func field(before, after any) history.FieldChange {
	return history.FieldChange{Before: mustJSON(before), After: mustJSON(after)}
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v) // strings and booleans always encode
	return raw
}

// BookArmed reports whether this database is the one the billing book was
// armed in.
func BookArmed(ctx context.Context, q *gen.Queries) (bool, error) {
	return q.GetBookArmed(ctx)
}

// ArmBook records this database as the billing book's one live copy.
func ArmBook(ctx context.Context, q *gen.Queries, by string, at time.Time) error {
	if by == "" {
		return fmt.Errorf("arming the book needs who armed it")
	}
	return q.ArmBook(ctx, gen.ArmBookParams{ArmedBy: by, ArmedAt: at.UTC()})
}
