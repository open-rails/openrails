package operator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
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
	ProviderAccountCutoverCode        = subscriptions.ProviderAccountCutoverCode
	ProviderAccountCutoverPlan        = subscriptions.ProviderAccountCutoverPlan
)

const (
	ProviderAccountCutoverSameAccount     = subscriptions.ProviderAccountCutoverSameAccount
	ProviderAccountCutoverRequiresReentry = subscriptions.ProviderAccountCutoverRequiresReentry
	ProviderAccountCutoverBlocked         = subscriptions.ProviderAccountCutoverBlocked

	ProviderAccountCutoverReady                     = subscriptions.ProviderAccountCutoverReady
	ProviderAccountCutoverIdentityMissing           = subscriptions.ProviderAccountCutoverIdentityMissing
	ProviderAccountCutoverRailUnsupported           = subscriptions.ProviderAccountCutoverRailUnsupported
	ProviderAccountCutoverTargetRailMismatch        = subscriptions.ProviderAccountCutoverTargetRailMismatch
	ProviderAccountCutoverSubscriptionNotRebilling  = subscriptions.ProviderAccountCutoverSubscriptionNotRebilling
	ProviderAccountCutoverSubscriptionNotAtProvider = subscriptions.ProviderAccountCutoverSubscriptionNotAtProvider
	ProviderAccountCutoverTargetArchived            = subscriptions.ProviderAccountCutoverTargetArchived
	ProviderAccountCutoverSourceNotArchived         = subscriptions.ProviderAccountCutoverSourceNotArchived
	ProviderAccountCutoverReplacementCardRequired   = subscriptions.ProviderAccountCutoverReplacementCardRequired
	ProviderAccountCutoverReplacementCardNotFound   = subscriptions.ProviderAccountCutoverReplacementCardNotFound
	ProviderAccountCutoverReplacementCardNotOwned   = subscriptions.ProviderAccountCutoverReplacementCardNotOwned
	ProviderAccountCutoverReplacementCardUnusable   = subscriptions.ProviderAccountCutoverReplacementCardUnusable
	ProviderAccountCutoverReplacementCardPSP        = subscriptions.ProviderAccountCutoverReplacementCardPSP
	ProviderAccountCutoverCrossAccountNotQualified  = subscriptions.ProviderAccountCutoverCrossAccountNotQualified
)

var ErrProviderAccountCutoverNotQualified = subscriptions.ErrProviderAccountCutoverNotQualified

// ProviderAccountCutoverQuery names one subscriber's requested move by durable
// identities. The source account is the subscription's own PSP. The target is
// TargetPSPID when given, else the replacement card's PSP; at least one of the
// two is required. Give both to check a re-entered card against the intended
// target.
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
// with subscriptions.PlanProviderAccountCutover. Read-only. A subscription or
// target PSP the merchant does not have is an error; every readiness gap is a
// coded plan.
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
	if q.ReplacementPaymentMethodID == nil && q.TargetPSPID == nil {
		return ProviderAccountCutoverReport{}, errors.New("a replacement payment method or a target PSP is required")
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
		req := subscriptions.ProviderAccountCutoverRequest{
			Rail:                models.Rail(sub.Rail),
			Status:              models.SubscriptionStatus(sub.Status),
			HasRailSubscription: strings.TrimSpace(sub.RailSubscriptionID) != "",
			SourcePSPID:         sub.PspID,
		}
		if q.ReplacementPaymentMethodID != nil {
			r := &subscriptions.ProviderAccountCutoverReplacement{}
			pm, err := dbq.GetPaymentMethodByID(ctx, *q.ReplacementPaymentMethodID)
			switch {
			case err == nil:
				r.Found = true
				r.OwnedByPayer = pm.CustomerID == sub.CustomerID
				r.PSPID = pm.PspID
				r.Rail = models.Rail(pm.Rail)
				r.PSPVaulted = pm.Custodian == models.CustodianPSP && strings.TrimSpace(pm.RailCustomerRef) != ""
				r.Parked = pm.ParkedAt != nil
				req.TargetPSPID = pm.PspID
			case errors.Is(err, pgx.ErrNoRows):
			default:
				return fmt.Errorf("payment method %s: %w", *q.ReplacementPaymentMethodID, err)
			}
			req.Replacement = r
		}
		if q.TargetPSPID != nil {
			req.TargetPSPID = *q.TargetPSPID
		}
		if req.TargetPSPID == uuid.Nil {
			req.TargetPSPID = sub.PspID // replacement not found: its report says so
		}
		source, err := pspRow(ctx, dbq, merchantID, req.SourcePSPID)
		if err != nil {
			return err
		}
		target, err := pspRow(ctx, dbq, merchantID, req.TargetPSPID)
		if err != nil {
			return err
		}
		req.SourceArchived = source.Archived
		req.TargetRail, req.TargetArchived = models.Rail(target.Rail), target.Archived
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
