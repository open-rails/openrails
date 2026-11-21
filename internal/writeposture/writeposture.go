// Package writeposture is how much OpenRails may write to payment providers for
// one merchant: the lower of the process's provider_write_mode and the
// merchant's stored posture, and readonly in a database the billing book was
// never armed in. Two live copies of one book would bill its customers twice.
package writeposture

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Origin identifies who requested a provider mutation.
type Origin string

const (
	OriginUser     Origin = "user"
	OriginAdmin    Origin = "admin"
	OriginProvider Origin = "provider"
	OriginSystem   Origin = "system"
)

type originKey struct{}

func WithOrigin(ctx context.Context, origin Origin) context.Context {
	return context.WithValue(ctx, originKey{}, origin)
}

func originFromContext(ctx context.Context) Origin {
	if origin, ok := ctx.Value(originKey{}).(Origin); ok {
		return origin
	}
	return OriginSystem
}

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

// HoldReason returns the reason an operation must wait, or empty if allowed.
func (p Posture) HoldReason(origin Origin) string {
	switch origin {
	case OriginUser, OriginAdmin, OriginProvider, OriginSystem:
	default:
		return fmt.Sprintf("unknown intent origin %q", origin)
	}
	if p.ReadOnly() || p.Limited() && origin == OriginSystem {
		if p.Reason != "" {
			return p.Reason
		}
		return "provider writes held by " + p.Mode.String()
	}
	return ""
}

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
		return Posture{ReadOnly, "provider_write_mode=readonly blocks all provider writes: set provider_write_mode to full (or limited)"}
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

// Provider is the exact account's effective write posture. An absent account,
// a missing declaration of write authority, and failed reads all deny writes.
func (v View) Provider(ctx context.Context, merchantID, pspID uuid.UUID) Posture {
	if merchantID == uuid.Nil || pspID == uuid.Nil {
		return Posture{ReadOnly, "provider write requires an exact merchant and PSP"}
	}
	bound := v.Posture(ctx, merchantID)
	if bound.ReadOnly() {
		return bound
	}
	mode, err := v.DB.Gen(ctx).GetPSPWriteMode(ctx, gen.GetPSPWriteModeParams{MerchantID: merchantID, PspID: pspID})
	if err != nil {
		return Posture{ReadOnly, fmt.Sprintf("PSP write posture unreadable (%v)", err)}
	}
	account, _ := ParseMode(mode)
	if account < bound.Mode {
		return Posture{account, "the payment provider account's write mode is " + account.String()}
	}
	return bound
}

// ProviderGuard rechecks the selected account at the send boundary.
func (v View) ProviderGuard(merchantID, pspID uuid.UUID) func(context.Context) error {
	return func(ctx context.Context) error {
		if reason := v.Provider(ctx, merchantID, pspID).HoldReason(originFromContext(ctx)); reason != "" {
			return errors.New(reason)
		}
		return nil
	}
}

// GuardCurrentProvider checks a provider selected by the operation's trusted
// account resolution.
func (v View) GuardCurrentProvider(ctx context.Context) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return v.ProviderGuard(mid.UUID(), db.PSPIDFromContext(ctx))(ctx)
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
