// Package destructive is the brake on destructive convergence (mass local
// cancellation, entitlement revocation, the provider deletes they queue): each
// merchant's destructive_armed control, read before each destructive act,
// default disarmed.
package destructive

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Verdict is one gate evaluation.
type Verdict struct {
	// Allowed: destructive actions (local cancel + revoke, destructive provider
	// intents) may execute for this merchant.
	Allowed bool
	// EnforceArmed: this merchant's provider pull may run in enforce mode.
	// False means advisory (findings only, no mutations).
	EnforceArmed bool
	// Reason explains a false Allowed / EnforceArmed, for the operator log.
	Reason string
	// FirstPullCompletedAt is nil until this merchant has been surveyed once.
	FirstPullCompletedAt *time.Time
}

// Gate reads the merchant's destructive arming. Fail-closed by construction:
// a read error or a nil Gate denies.
type Gate struct {
	DB *db.DB
}

// New builds a gate over a database handle.
func New(database *db.DB) *Gate { return &Gate{DB: database} }

// Check evaluates the merchant's arming. ctx MUST already be merchant-scoped
// (inside RunInMerchantConn / MerchantTx). Use CheckMerchant when you are not.
func (g *Gate) Check(ctx context.Context, merchantID uuid.UUID) Verdict {
	if g == nil || g.DB == nil {
		return Verdict{Reason: "destructive gate not wired; refusing destructive actions (fail closed)"}
	}
	row, err := g.DB.Gen(ctx).GetMerchantControls(ctx, merchantID)
	if err != nil {
		return Verdict{Reason: fmt.Sprintf("merchant controls unreadable (%v); refusing destructive actions (fail closed)", err)}
	}
	return verdict(merchantID, row.DestructiveArmed, row.FirstPullCompletedAt)
}

// verdict is the gate's answer for one merchant's arming: one control gates
// destructive convergence and enforcing pulls alike.
func verdict(merchantID uuid.UUID, armed bool, firstPull *time.Time) Verdict {
	if !armed {
		return Verdict{
			Reason: fmt.Sprintf("destructive actions are not armed for merchant %s: its pulls run ADVISORY (findings are persisted, nothing is mutated) and no cancellation, revocation or provider delete executes. "+
				"Review its findings, then `openrails merchant destructive arm`", merchantID),
			FirstPullCompletedAt: firstPull,
		}
	}
	return Verdict{Allowed: true, EnforceArmed: true, FirstPullCompletedAt: firstPull}
}

// CheckMerchant is Check for callers that are NOT already on a merchant-scoped
// connection (the intent runner, cross-merchant schedulers): it opens one.
func (g *Gate) CheckMerchant(ctx context.Context, merchantID uuid.UUID) Verdict {
	if g == nil || g.DB == nil {
		return Verdict{Reason: "destructive gate not wired; refusing destructive actions (fail closed)"}
	}
	var v Verdict
	mctx := merchant.WithID(ctx, billing.MerchantID(merchantID))
	if err := g.DB.RunInMerchantConn(mctx, func(sctx context.Context) error {
		v = g.Check(sctx, merchantID)
		return nil
	}); err != nil {
		return Verdict{Reason: fmt.Sprintf("merchant controls unreadable (%v); refusing destructive actions (fail closed)", err)}
	}
	return v
}

// RecordFirstPull stamps that a completed pull surveyed a merchant, so its
// findings are ready to review. Best-effort. ctx must be merchant-scoped.
func (g *Gate) RecordFirstPull(ctx context.Context, merchantID uuid.UUID, now time.Time) error {
	if g == nil || g.DB == nil {
		return nil
	}
	return g.DB.Gen(ctx).RecordFirstPullCompleted(ctx, gen.RecordFirstPullCompletedParams{
		MerchantID: merchantID, CompletedAt: now.UTC(),
	})
}

// EvidenceFloor is the instant this deployment first completed a pull for a
// merchant: a destructive decision may not rest on evidence older than this.
// Zero (nil gate, unreadable controls, never pulled) is not permissive: the
// decider then trusts only this pass's evidence. ctx must be merchant-scoped.
func (g *Gate) EvidenceFloor(ctx context.Context, merchantID uuid.UUID) time.Time {
	if g == nil || g.DB == nil {
		return time.Time{}
	}
	row, err := g.DB.Gen(ctx).GetMerchantControls(ctx, merchantID)
	if err != nil || row.FirstPullCompletedAt == nil {
		return time.Time{}
	}
	return row.FirstPullCompletedAt.UTC()
}

// AllowDestructive implements the intents runner's gate: it opens its own
// merchant-scoped read, and fails closed.
func (g *Gate) AllowDestructive(ctx context.Context, merchantID uuid.UUID) (bool, string) {
	v := g.CheckMerchant(ctx, merchantID)
	return v.Allowed, v.Reason
}
