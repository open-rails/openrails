// Package writeposture is how much OpenRails may write to payment providers for
// one merchant: the lower of the process's provider_write_mode and the
// merchant's stored posture, and readonly in a database the billing book was
// never armed in. Two live copies of one book would bill its customers twice.
package writeposture

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
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
	case config.ProviderWriteModeFull:
		return Full, true
	case config.ProviderWriteModeLimited:
		return Limited, true
	case config.ProviderWriteModeReadOnly:
		return ReadOnly, true
	}
	return ReadOnly, false
}

func (m Mode) String() string {
	switch m {
	case Full:
		return config.ProviderWriteModeFull
	case Limited:
		return config.ProviderWriteModeLimited
	}
	return config.ProviderWriteModeReadOnly
}

// Reasons a merchant's stored posture was set.
const (
	ReasonExported = "exported"
	ReasonRestored = "restored"
	ReasonOperator = "operator"
)

// Posture is one merchant's effective write posture.
type Posture struct {
	Mode Mode
	// Reason says why writes wait; empty at Full.
	Reason string
}

// ReadOnly: every provider write waits.
func (p Posture) ReadOnly() bool { return p.Mode == ReadOnly }

// Limited: proactive (system-origin) provider writes wait.
func (p Posture) Limited() bool { return p.Mode < Full }

// View reads merchants' postures. It fails closed: without a Config or a
// database, or when the posture cannot be read, the posture is readonly.
type View struct {
	Config *config.Config
	DB     *db.DB
}

// Posture is merchantID's effective posture.
func (v View) Posture(ctx context.Context, merchantID uuid.UUID) Posture {
	if v.Config == nil {
		return Posture{ReadOnly, "operating mode is unknown (no config wired)"}
	}
	process, _ := ParseMode(config.GetProviderWriteMode(v.Config))
	if process == ReadOnly {
		return Posture{ReadOnly, "provider_write_mode=readonly blocks all provider writes"}
	}
	if v.DB == nil {
		return Posture{ReadOnly, "write posture unreadable (no database wired)"}
	}
	row, err := v.DB.Gen(ctx).GetWritePosture(ctx, merchantID)
	if err != nil {
		return Posture{ReadOnly, fmt.Sprintf("write posture unreadable (%v)", err)}
	}
	return effective(process, row)
}

func effective(process Mode, row gen.GetWritePostureRow) Posture {
	if !row.BookArmed {
		return Posture{ReadOnly, "this database is a copy of the billing book: run `openrails book arm` once it is the only live copy"}
	}
	stored, ok := ParseMode(row.Mode)
	if !ok {
		return Posture{ReadOnly, fmt.Sprintf("unknown stored write posture %q", row.Mode)}
	}
	if stored < process {
		return Posture{stored, storedReason(row)}
	}
	if process == Limited {
		return Posture{Limited, "provider_write_mode=limited blocks proactive (system-origin) provider writes"}
	}
	return Posture{Mode: Full}
}

func storedReason(row gen.GetWritePostureRow) string {
	reason := "the merchant's write posture is " + row.Mode
	if row.Reason != nil {
		reason += " (" + *row.Reason + ")"
	}
	return reason + ": `openrails merchant arm` restores it"
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

// Set stores merchantID's posture.
func Set(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, mode Mode, reason, by string, at time.Time) error {
	if by == "" {
		return fmt.Errorf("setting a write posture needs who set it")
	}
	return q.SetWritePosture(ctx, gen.SetWritePostureParams{MerchantID: merchantID, Mode: mode.String(), Reason: reason, SetBy: by, SetAt: at.UTC()})
}
