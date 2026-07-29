// Package destructive is the operator control plane for OpenRails' destructive
// convergence: mass local cancellation, entitlement revocation, and the
// irreversible provider-side deletes they queue.
//
// #836: before this, an operator watching a mass cancellation at 3am could not
// stop it without a deploy. `provider_write_mode: readonly` is config/env only
// — no SIGHUP, no config watch — River's QueuePause is unwired, and the #679
// volume breaker holds provider deletes but never the LOCAL cancel + revoke
// that actually takes a customer's access away. The Gate is the missing brake:
// a DB-backed flag, read at the top of every destructive plane, flippable with
// one UPDATE on any node, default SAFE.
package destructive

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Verdict is one gate evaluation.
type Verdict struct {
	// Allowed: destructive actions (local cancel + revoke, destructive provider
	// intents) may execute for this merchant.
	Allowed bool
	// EnforceArmed (#835): this merchant's provider pull may run in ENFORCE
	// mode. False means the pull runs advisory — findings only, zero mutations
	// — until an operator reviews the first pull and arms the merchant.
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
// half is RLS-protected tenant data. Use CheckMerchant when you are not.
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
			Reason:               "instance kill switch is OFF (openrails.destructive_action_switch.enabled = false): no local cancellation, entitlement revocation or provider delete will execute on any node",
			FirstPullCompletedAt: row.FirstPullCompletedAt,
		}
	case !row.MerchantEnabled:
		return Verdict{
			Reason:               fmt.Sprintf("destructive actions are disabled for merchant %s (openrails.merchant_destructive_policy.destructive_actions_enabled = false)", merchantID),
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
	mctx := merchant.WithID(ctx, merchant.ID(merchantID))
	if err := g.DB.RunInMerchantConn(mctx, func(sctx context.Context) error {
		v = g.Check(sctx, merchantID)
		return nil
	}); err != nil {
		return Verdict{Reason: fmt.Sprintf("destructive policy unreadable (%v); refusing destructive actions (fail closed)", err)}
	}
	return v
}

// RecordFirstPull stamps that a merchant has been surveyed by a completed pull
// (#835), so an operator can see its findings are ready to review. Best-effort:
// never fails a pass. ctx must be merchant-scoped.
func (g *Gate) RecordFirstPull(ctx context.Context, merchantID uuid.UUID, now time.Time) error {
	if g == nil || g.DB == nil {
		return nil
	}
	return g.DB.Gen(ctx).RecordFirstPullCompleted(ctx, gen.RecordFirstPullCompletedParams{
		MerchantID: merchantID, CompletedAt: now.UTC(),
	})
}

// EvidenceFloor is the #835 staleness floor for a merchant: the instant this
// deployment first completed a pull for it. A destructive decision may not rest
// on evidence older than this — nothing older was ever corroborated by an
// observation we made.
//
// Zero on a nil gate, an unreadable policy, or a merchant that has never been
// pulled. Zero is NOT permissive: the decider then trusts only the evidence the
// current pass observed. ctx must be merchant-scoped.
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

// Arm is the #835 operator flip: bless a merchant for enforcing pulls. ctx must
// be merchant-scoped.
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
