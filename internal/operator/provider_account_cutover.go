package operator

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Provider-account cutover reports are report-only (#657): they classify one
// subscriber's move between provider accounts and enumerate the proof a
// cross-account execution would need. Nothing here writes, arms a rail or
// touches the provider; cross-PSP execution stays disabled until a
// provider-specific create/verify/cancel/repoint contract is qualified.
type (
	ProviderAccountCutoverDisposition = subscriptions.ProviderAccountCutoverDisposition
	ProviderAccountCutoverPlan        = subscriptions.ProviderAccountCutoverPlan
)

const (
	ProviderAccountCutoverSameAccount     = subscriptions.ProviderAccountCutoverSameAccount
	ProviderAccountCutoverRequiresReentry = subscriptions.ProviderAccountCutoverRequiresReentry
	ProviderAccountCutoverBlocked         = subscriptions.ProviderAccountCutoverBlocked
)

var ErrProviderAccountCutoverNotQualified = subscriptions.ErrProviderAccountCutoverNotQualified

// ProviderAccountCutoverQuery names one subscriber's requested move by durable
// identities. The source account is the subscription's own PSP. The target is
// the replacement card's PSP when the card has already been re-entered, else
// the named TargetPSPID; exactly one of the two is given.
type ProviderAccountCutoverQuery struct {
	SubscriptionID             uuid.UUID
	ReplacementPaymentMethodID *uuid.UUID
	TargetPSPID                *uuid.UUID
}

// ProviderAccountCutoverReport is the resolved plan with the identities it was
// computed from. Safe to serialize into a runbook; never a completed cutover.
type ProviderAccountCutoverReport struct {
	SubscriptionID uuid.UUID                  `json:"subscription_id"`
	SourcePSPID    uuid.UUID                  `json:"source_psp_id"`
	TargetPSPID    uuid.UUID                  `json:"target_psp_id"`
	Plan           ProviderAccountCutoverPlan `json:"plan"`
}

// PlanProviderAccountCutover resolves the subscription, the optional replacement
// method and both PSP rows under the merchant's scope, then classifies the move
// with subscriptions.PlanProviderAccountCutover. Read-only.
func PlanProviderAccountCutover(ctx context.Context, a *app.App, merchantID merchant.ID, q ProviderAccountCutoverQuery) (ProviderAccountCutoverReport, error) {
	if Get(a) == nil {
		return ProviderAccountCutoverReport{}, errors.New("no control plane attached (call Attach first)")
	}
	if a.Runtime == nil || a.Runtime.DB == nil {
		return ProviderAccountCutoverReport{}, errors.New("runtime unavailable")
	}
	if merchantID.IsZero() || q.SubscriptionID == uuid.Nil {
		return ProviderAccountCutoverReport{}, errors.New("merchant and subscription are required")
	}
	if (q.ReplacementPaymentMethodID == nil) == (q.TargetPSPID == nil) {
		return ProviderAccountCutoverReport{}, errors.New("exactly one of replacement payment method or target PSP is required")
	}
	report := ProviderAccountCutoverReport{SubscriptionID: q.SubscriptionID}
	err := a.Runtime.DB.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		dbq := a.Runtime.DB.Gen(ctx)
		sub, err := dbq.GetSubscriptionByID(ctx, q.SubscriptionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("subscription %s: not found", q.SubscriptionID)
			}
			return fmt.Errorf("subscription %s: %w", q.SubscriptionID, err)
		}
		req := subscriptions.ProviderAccountCutoverRequest{SourcePSPID: sub.PspID}
		if q.ReplacementPaymentMethodID != nil {
			pm, err := dbq.GetPaymentMethodByID(ctx, *q.ReplacementPaymentMethodID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("payment method %s: not found", *q.ReplacementPaymentMethodID)
				}
				return fmt.Errorf("payment method %s: %w", *q.ReplacementPaymentMethodID, err)
			}
			if pm.CustomerID != sub.CustomerID {
				return fmt.Errorf("payment method %s belongs to another customer than subscription %s", pm.ID, sub.ID)
			}
			req.TargetPSPID, req.ReplacementCardCollected = pm.PspID, true
		} else {
			req.TargetPSPID = *q.TargetPSPID
		}
		source, err := pspRow(ctx, dbq, merchantID, req.SourcePSPID)
		if err != nil {
			return err
		}
		target, err := pspRow(ctx, dbq, merchantID, req.TargetPSPID)
		if err != nil {
			return err
		}
		req.SourceArchived, req.TargetArchived = source.Archived, target.Archived
		report.SourcePSPID, report.TargetPSPID = req.SourcePSPID, req.TargetPSPID
		report.Plan = subscriptions.PlanProviderAccountCutover(req)
		return nil
	})
	return report, err
}

func pspRow(ctx context.Context, q *gen.Queries, merchantID merchant.ID, id uuid.UUID) (gen.OpenrailsPsp, error) {
	psp, err := q.GetPSP(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return gen.OpenrailsPsp{}, fmt.Errorf("psp %s: not found", id)
		}
		return gen.OpenrailsPsp{}, fmt.Errorf("psp %s: %w", id, err)
	}
	if psp.MerchantID != merchantID.UUID() {
		return gen.OpenrailsPsp{}, fmt.Errorf("psp %s: not found", id)
	}
	return psp, nil
}
