package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// nmiAdapter implements providerAdapter for NMI recurring plans (Direct Post +
// Query APIs). The link's provider field records the PSP key the plan lives
// under.
//
//   - Attach: find-or-create at the operator-supplied plan_id: an existing plan
//     must match the price's amount and frequency; a missing one is created
//     from the price's terms.
//   - AutoCreate: the same at `or-<32-hex-UUID>`; needs a day cadence. With no
//     NMI rail armed it returns errPendingManualLink.
//   - Update: no-op; amount/frequency are immutable and is_active is not
//     representable.
//   - Verify: live read, diffs unit_amount.
//
// Archiving a price never deletes its NMI plan: a deleted plan does not stop
// subscriptions billing on it, so the plan must outlive the price.
type nmiAdapter struct {
	svc *Service
	// testEndpointURL points built NMI clients at a fake gateway (test seam).
	testEndpointURL string
}

func (a *nmiAdapter) Name() string { return string(models.RailNMI) }

func (a *nmiAdapter) PendingActionTemplate(priceID uuid.UUID) billing.PendingAction {
	return billing.PendingAction{
		PSP:    string(models.RailNMI),
		Action: "create_recurring_plan",
		Hint:   "Create plan in NMI control center, then PATCH /admin/catalog/prices/" + priceID.String() + " with psp_links.nmi.plan_id",
		PatchRequired: map[string]map[string]map[string]string{
			"psp_links": {
				string(models.RailNMI): {
					"plan_id": "<plan id>",
				},
			},
		},
	}
}

func (a *nmiAdapter) Attach(ctx context.Context, link map[string]string, in autoCreateContext) (map[string]string, error) {
	if err := moneyutil.RequireFiatCurrency(in.Currency); err != nil {
		return nil, err
	}
	if in.BillingIntervalHours != nil && *in.BillingIntervalHours%24 != 0 {
		return nil, fmt.Errorf("nmi recurring prices require a whole-day billing interval")
	}
	link = normalizeLinkMap(link)
	planID := strings.TrimSpace(link[models.RailKeyPlanID])
	if planID == "" {
		return nil, fmt.Errorf("nmi link requires psp_links.nmi.plan_id")
	}
	// provider is the PSP key the plan lives under (metadata only): the link's
	// override, else the armed account's key.
	provider := strings.ToLower(strings.TrimSpace(link[models.RailKeyProvider]))
	client, pspKey, ok := a.nmiClient(ctx)
	if provider == "" {
		provider = pspKey
	}

	// NMI plan_ids are operator-chosen and client-creatable, so an explicit link
	// is a find-or-create at that id: an existing plan must match the price's
	// money terms (a mismatch is a loud error); a missing one is created. With
	// no NMI rail armed the link is stored as-is (operator-owned).
	if ok && client != nil {
		detail, err := client.GetRecurringPlanDetailByID(ctx, planID, in.Currency)
		if err != nil {
			return nil, fmt.Errorf("verify NMI recurring plan %q: %w", planID, err)
		}
		if detail.Found {
			remoteAmount, err := moneyutil.RailMinorToNative(in.Currency, moneyutil.Cents(detail.AmountCents))
			if err != nil {
				return nil, err
			}
			if in.UnitAmount > 0 && remoteAmount != in.UnitAmount {
				return nil, fmt.Errorf("NMI recurring plan %q amount (%s) does not match catalog price (%s)", planID, moneyutil.FormatAmount(remoteAmount, in.Currency), moneyutil.FormatAmount(in.UnitAmount, in.Currency))
			}
			// day_frequency is only reported for day-based plans; validate it only
			// when NMI returns one (month-based plans report 0 -> unverifiable here).
			if in.BillingCycleDays != nil && *in.BillingCycleDays > 0 && detail.DayFrequency > 0 && detail.DayFrequency != *in.BillingCycleDays {
				return nil, fmt.Errorf("NMI recurring plan %q billing cycle (%s) does not match catalog price (%s)", planID, cadence.FormatHours(detail.DayFrequency*24), cadence.FormatHours(*in.BillingCycleDays*24))
			}
		} else {
			if in.RemoteWritesDisabled {
				return nil, fmt.Errorf("NMI recurring plan %q does not exist: %w", planID, errRemoteWritesDisabled)
			}
			if err := a.createPlan(ctx, client, planID, in); err != nil {
				return nil, fmt.Errorf("link plan_id %q does not exist and could not be created: %w", planID, err)
			}
		}
	}

	out := map[string]string{models.RailKeyPlanID: planID}
	if provider != "" {
		out[models.RailKeyProvider] = provider
	}
	return out, nil
}

// createPlan adds an NMI Recurring Plan at planID from the price's money terms
// (AutoCreate and Attach). NMI plans need a fixed frequency and a positive
// amount.
func (a *nmiAdapter) createPlan(ctx context.Context, client *nmi.NMIClient, planID string, in autoCreateContext) error {
	if in.RemoteWritesDisabled {
		return errRemoteWritesDisabled
	}
	if in.BillingCycleDays == nil || *in.BillingCycleDays <= 0 {
		return fmt.Errorf("recurring day cadence is required (NMI plans need a recurring frequency)")
	}
	if in.UnitAmount <= 0 {
		return fmt.Errorf("a positive unit_amount is required")
	}
	amountCents, err := moneyutil.NativeToRailMinorExact(in.Currency, in.UnitAmount)
	if err != nil {
		return err
	}
	planName := ""
	if in.Product != nil {
		planName = strings.TrimSpace(in.Product.DisplayName)
	}
	if planName == "" {
		planName = planID
	}
	// plan_payments=0 means bill forever; OpenRails models open-ended subscriptions.
	return client.AddRecurringPlan(ctx, planID, planName, moneyutil.Cents(amountCents), in.Currency, *in.BillingCycleDays, 0)
}

// nmiDeterministicPlanID addresses the immutable local price. Its short
// prefixed UUID distinguishes sibling keys and all versions of their terms.
// Explicitly attached operator plan IDs are preserved unchanged.
func nmiDeterministicPlanID(priceID uuid.UUID) string {
	return "or-" + strings.ReplaceAll(priceID.String(), "-", "")
}

// nmiClient resolves the ctx merchant's active NMI client; see nmiClientFor.
func (a *nmiAdapter) nmiClient(ctx context.Context) (*nmi.NMIClient, string, bool) {
	return a.nmiClientFor(ctx, "")
}

// nmiClientFor arms the NMI client from the ctx merchant's armed rail state:
// empty targetAccountID is the active account, else that declared account.
// pspKey is the resolved account's PSP key. ok=false = not armed; callers defer
// to a manual link or sync_disabled, never another account.
func (a *nmiAdapter) nmiClientFor(ctx context.Context, targetAccountID string) (client *nmi.NMIClient, pspKey string, ok bool) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.RailConfigs == nil {
		return nil, "", false
	}
	proc, err := a.svc.rt.RailConfigs.RailConfig(ctx, string(models.RailNMI), strings.TrimSpace(targetAccountID))
	if err != nil || proc == nil || proc.NMI == nil || strings.TrimSpace(proc.NMI.SecurityKey) == "" {
		return nil, "", false
	}
	mid, merr := merchant.Require(ctx)
	if merr != nil || proc.ID == uuid.Nil {
		return nil, "", false
	}
	// The runtime's one NMI factory carries its transport; a private factory
	// would reach the real gateway around it.
	factory := railresolve.NMIFactory{Config: a.svc.rt.Config}
	if a.svc.rt.NMIClients != nil {
		factory = *a.svc.rt.NMIClients
	}
	if a.testEndpointURL != "" {
		factory.Endpoints = railresolve.LoopbackNMIEndpoints(a.testEndpointURL)
	}
	client, err = factory.ClientFor(mid, merchants.PSPScope{ID: proc.ID, Rail: string(models.RailNMI), AccountID: proc.EffectiveAccountID(), Key: proc.Key}, proc.ToNMIProviderSettings())
	if err != nil {
		return nil, "", false
	}
	return client, proc.Key, true
}

// AutoCreate creates (or attaches to) the NMI Recurring Plan for a price under a
// deterministic plan_id. When no NMI rail is configured it returns
// errPendingManualLink so the dispatcher converts the slot to a manual link.
func (a *nmiAdapter) AutoCreate(ctx context.Context, in autoCreateContext) (map[string]string, error) {
	if err := moneyutil.RequireFiatCurrency(in.Currency); err != nil {
		return nil, err
	}
	if in.BillingIntervalHours != nil && *in.BillingIntervalHours%24 != 0 {
		return nil, fmt.Errorf("nmi recurring prices require a whole-day billing interval")
	}
	client, pspKey, ok := a.nmiClientFor(ctx, in.TargetAccountID)
	if !ok || client == nil {
		// No NMI rail configured (or unknown target account): defer to manual link.
		return nil, errPendingManualLink
	}
	// NMI recurring plans require a fixed billing frequency.
	if in.BillingCycleDays == nil || *in.BillingCycleDays <= 0 {
		return nil, fmt.Errorf("nmi create-mode requires recurring day cadence (NMI plans need a recurring frequency)")
	}

	if in.PriceID == uuid.Nil {
		return nil, fmt.Errorf("nmi auto-create requires a local price ID")
	}
	planID := nmiDeterministicPlanID(in.PriceID)

	// Find-or-create: prefer an existing plan with this deterministic id.
	detail, err := client.GetRecurringPlanDetailByID(ctx, planID, in.Currency)
	if err != nil {
		return nil, fmt.Errorf("lookup recurring plan: %w", err)
	}
	if detail.Found {
		remoteAmount, err := moneyutil.RailMinorToNative(in.Currency, moneyutil.Cents(detail.AmountCents))
		if err != nil {
			return nil, err
		}
		if remoteAmount != in.UnitAmount || detail.DayFrequency > 0 && detail.DayFrequency != *in.BillingCycleDays {
			return nil, fmt.Errorf("NMI recurring plan %q does not match the local price terms", planID)
		}
	} else {
		if err := a.createPlan(ctx, client, planID, in); err != nil {
			return nil, fmt.Errorf("create recurring plan: %w", err)
		}
	}

	out := map[string]string{models.RailKeyPlanID: planID}
	if pspKey != "" {
		out[models.RailKeyProvider] = pspKey
	}
	return out, nil
}

// Verify performs a live retrieve of the NMI plan and computes unit_amount drift.
func (a *nmiAdapter) Verify(ctx context.Context, ids map[string]string, local *priceVerifyContext) ([]billing.DriftField, bool, error) {
	if local != nil && local.Currency != "" {
		if err := moneyutil.RequireFiatCurrency(local.Currency); err != nil {
			return nil, false, err
		}
	}
	client, _, ok := a.nmiClient(ctx)
	if !ok || client == nil {
		// No readable account is not agreement: signal sync_disabled.
		return nil, false, fmt.Errorf("nmi is not configured: %w", errProviderNotArmed)
	}
	planID := strings.TrimSpace(ids[models.RailKeyPlanID])
	if planID == "" {
		return nil, false, fmt.Errorf("nmi plan_id missing on local rails map")
	}
	if local == nil || strings.TrimSpace(local.Currency) == "" {
		return nil, false, fmt.Errorf("NMI plan verification requires the established catalog currency")
	}
	found, _, remoteAmountCents, err := client.GetRecurringPlanByID(ctx, planID, local.Currency)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, true, nil
	}
	drift := []billing.DriftField{}
	if local != nil {
		remoteAmountMicros, err := moneyutil.RailMinorToNative(local.Currency, moneyutil.Cents(remoteAmountCents))
		if err != nil {
			return nil, false, err
		}
		if local.UnitAmount != remoteAmountMicros {
			drift = append(drift, billing.DriftField{
				Field:          "unit_amount",
				OpenRailsValue: strconv.FormatInt(local.UnitAmount, 10),
				RemoteValue:    strconv.FormatInt(remoteAmountMicros, 10),
			})
		}
	}
	return drift, false, nil
}

// Update is a no-op: NMI plans cannot represent is_active, and amount/frequency
// are immutable after create.
func (a *nmiAdapter) Update(_ context.Context, _ map[string]string, _ mutableUpdate) error {
	return nil
}
