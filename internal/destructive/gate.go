// Package destructive is the brake on destructive convergence (mass local
// cancellation, entitlement revocation, the provider deletes they queue): a
// DB-backed switch read at the top of every destructive plane, default safe.
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
	// False means advisory (findings only, no mutations) until an operator
	// reviews the first pull and arms the merchant.
	EnforceArmed bool
	// Reason explains a false Allowed / EnforceArmed, for the operator log.
	Reason string
	// FirstPullCompletedAt is nil until this merchant has been surveyed once.
	FirstPullCompletedAt *time.Time
}

// Gate reads the destructive-action policy. Fail-closed by construction: a
// read error, a missing switch row, or a nil Gate all deny.
type Gate struct {
	DB *db.DB
}

// New builds a gate over a database handle.
func New(database *db.DB) *Gate { return &Gate{DB: database} }

// Check evaluates the policy for a merchant. ctx MUST already be
// merchant-scoped (inside RunInMerchantConn / MerchantTx): the per-merchant
// half is merchant-owned tenant data. Use CheckMerchant when you are not.
func (g *Gate) Check(ctx context.Context, merchantID uuid.UUID) Verdict {
	if g == nil || g.DB == nil {
		return Verdict{Reason: "destructive gate not wired; refusing destructive actions (fail closed)"}
	}
	row, err := g.DB.Gen(ctx).GetDestructivePolicy(ctx, merchantID)
	if err != nil {
		return Verdict{Reason: fmt.Sprintf("destructive policy unreadable (%v); refusing destructive actions (fail closed)", err)}
	}
	switch {
	case !row.SwitchEnabled:
		return Verdict{
			Reason:               "instance kill switch is OFF (billing.destructive_action_switch.enabled = false): no maintenance cancellation, entitlement revocation or operator/system provider delete will execute on any node",
			FirstPullCompletedAt: row.FirstPullCompletedAt,
		}
	case !row.MerchantEnabled:
		return Verdict{
			Reason:               fmt.Sprintf("destructive actions are disabled for merchant %s (billing.merchant_destructive_policy.destructive_actions_enabled = false)", merchantID),
			FirstPullCompletedAt: row.FirstPullCompletedAt,
		}
	}
	v := Verdict{Allowed: true, EnforceArmed: row.EnforceArmedAt != nil, FirstPullCompletedAt: row.FirstPullCompletedAt}
	if !v.EnforceArmed {
		v.Reason = fmt.Sprintf("merchant %s has never been armed for enforcing pulls (#835): this pass runs ADVISORY — findings are persisted, nothing is mutated. Review them, then ArmMerchantEnforcement", merchantID)
	}
	return v
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
		return Verdict{Reason: fmt.Sprintf("destructive policy unreadable (%v); refusing destructive actions (fail closed)", err)}
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
// Zero (nil gate, unreadable policy, never pulled) is not permissive: the
// decider then trusts only this pass's evidence. ctx must be merchant-scoped.
func (g *Gate) EvidenceFloor(ctx context.Context, merchantID uuid.UUID) time.Time {
	if g == nil || g.DB == nil {
		return time.Time{}
	}
	row, err := g.DB.Gen(ctx).GetDestructivePolicy(ctx, merchantID)
	if err != nil || row.FirstPullCompletedAt == nil {
		return time.Time{}
	}
	return row.FirstPullCompletedAt.UTC()
}

// Arm blesses a merchant for enforcing pulls. ctx must be merchant-scoped.
func (g *Gate) Arm(ctx context.Context, merchantID uuid.UUID, now time.Time, by, reason string) error {
	if g == nil || g.DB == nil {
		return fmt.Errorf("destructive gate not wired")
	}
	return g.DB.Gen(ctx).ArmMerchantEnforcement(ctx, gen.ArmMerchantEnforcementParams{
		MerchantID: merchantID, ArmedAt: now.UTC(),
		UpdatedBy: strPtr(by), Reason: strPtr(reason),
	})
}

// SetSwitch flips the instance kill switch. Runs on any connection.
func (g *Gate) SetSwitch(ctx context.Context, enabled bool, by, reason string) error {
	if g == nil || g.DB == nil {
		return fmt.Errorf("destructive gate not wired")
	}
	return g.DB.Gen(ctx).SetDestructiveActionSwitch(ctx, gen.SetDestructiveActionSwitchParams{
		Enabled: enabled, UpdatedBy: strPtr(by), Reason: strPtr(reason),
	})
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AllowDestructive implements the intents runner's gate: it opens its own
// merchant-scoped read, and fails closed.
func (g *Gate) AllowDestructive(ctx context.Context, merchantID uuid.UUID) (bool, string) {
	v := g.CheckMerchant(ctx, merchantID)
	return v.Allowed, v.Reason
}
