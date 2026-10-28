package reconcile

import (
	"context"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
)

// Every convergence cancel ends access, so every one passes this brake inside
// ApplyDecision, whatever the rail or caller. A pull-plane pass (verification,
// the unknown cohort) decides its rows first and declares its planned cancels
// once: the destructive switch and enforce arming, then the cancel budget
// against the PSP's live book, all or nothing; a cancel it did not plan (a
// re-decision after a concurrent write) is held. Any other caller is a pass of
// one, held by the destructive switch; one cancel is always within budget. The
// switch is read again for every cancel, so turning it off stops a running
// pass. An admitted operator command (a declared book, an approved proposal)
// carries its own authority. A held cancel parks a live row `unverified`, access
// kept, for the next read once the gate allows it.

type cancelPassKey struct{}

type cancelPass struct {
	held     bool
	reason   string
	admitted bool
}

// GateCancelPass evaluates one pull-plane pass's planned cancels for a PSP and
// returns the context its decisions apply under. A budget overrun raises the
// cancellation-cap finding. ctx must be merchant-scoped.
func GateCancelPass(ctx context.Context, database *db.DB, mid billing.MerchantID, provider Provider, pspID uuid.UUID, planned int) context.Context {
	pass := cancelPass{held: true, reason: "the pass planned no cancellation"}
	if planned > 0 {
		pass = evaluateCancelPass(ctx, database, mid, provider, pspID, planned)
	}
	return context.WithValue(ctx, cancelPassKey{}, pass)
}

// WithAdmittedCancels marks ctx as an operator command already admitted: its
// cancels apply without another gate.
func WithAdmittedCancels(ctx context.Context) context.Context {
	return context.WithValue(ctx, cancelPassKey{}, cancelPass{admitted: true})
}

// CancelsHeld reports whether the pass bound to ctx holds its cancels.
func CancelsHeld(ctx context.Context) bool {
	pass, ok := ctx.Value(cancelPassKey{}).(cancelPass)
	return ok && pass.held
}

func evaluateCancelPass(ctx context.Context, database *db.DB, mid billing.MerchantID, provider Provider, pspID uuid.UUID, planned int) cancelPass {
	if v := destructive.New(database).Check(ctx, mid.UUID()); !v.Allowed || !v.EnforceArmed {
		return cancelPass{held: true, reason: v.Reason}
	}
	q := database.Gen(ctx)
	live, err := q.CountLiveLinkedSubscriptionsForPSP(ctx, gen.CountLiveLinkedSubscriptionsForPSPParams{MerchantID: mid.UUID(), PspID: pspID})
	if err != nil {
		return cancelPass{held: true, reason: "the live book could not be counted: " + err.Error()}
	}
	if exceeded, reason := (CancelBudget{}).Exceeded(planned, int(live)); exceeded {
		recordGuardFinding(ctx, q, mid, provider, "cancellation_cap", reason)
		return cancelPass{held: true, reason: reason}
	}
	return cancelPass{}
}

// cancelHeld is ApplyDecision's brake on one cancel.
func cancelHeld(ctx context.Context, database *db.DB, sub *models.Subscription, d Decision) bool {
	pass, _ := ctx.Value(cancelPassKey{}).(cancelPass)
	if !pass.held && !pass.admitted {
		if v := destructive.New(database).Check(ctx, sub.MerchantID); !v.Allowed {
			pass = cancelPass{held: true, reason: v.Reason}
		}
	}
	if pass.held {
		log.WithContext(ctx).WithFields(log.Fields{"subscription_id": sub.ID, "rail": sub.Rail, "reason": d.Reason, "held_by": pass.reason}).
			Warn("convergence cancel held; the subscription keeps its access")
	}
	return pass.held
}
