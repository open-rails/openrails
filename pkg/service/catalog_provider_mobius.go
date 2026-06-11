package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// mobiusAdapter implements providerAdapter for the Mobius (NMI-backed)
// recurring-plan provider.
//
// As of issue #207, NMI is a first-class create-capable provider: OpenRails
// programmatically creates, attaches to, updates, and reconciles NMI Recurring
// Plans via the Direct Post + Query APIs.
//
//   - Attach: find-or-create at the operator-supplied plan_id. NMI plan_ids are
//     operator-chosen AND client-creatable, so a link to a plan that exists is
//     verified (amount + frequency must match the price) while a link to one that
//     does not yet exist is CREATED from the price's terms (+ optional provider override).
//   - AutoCreate: content-addressed plan_id `<slug>-<cur>-<amt>-<cycle>`; find-or-attach
//     against NMI, falling back to creating the plan. Requires
//     billing_cycle_days (NMI requires a frequency). When no NMI processor is
//     configured, falls back to errPendingManualLink so the operator can link
//     a control-center plan manually.
//   - Update: propagates display_name via EditRecurringPlan; rejects financial
//     changes (amount/frequency are immutable post-create in NMI).
//   - Verify: live GetRecurringPlanByID + diff plan_name.
//
// Divergence from Stripe: on price deactivation OpenRails does NOT delete the
// NMI plan. NMI delete_recurring_plan does not stop subscriptions already
// billing on the plan, so the plan must outlive the OpenRails price.
type mobiusAdapter struct {
	svc *Service
}

func (a *mobiusAdapter) Name() string { return "mobius" }

func (a *mobiusAdapter) PendingActionTemplate(priceID uuid.UUID) PendingAction {
	return PendingAction{
		Provider: "mobius",
		Action:   "create_recurring_plan",
		Hint:     "Create plan in NMI control center, then PATCH /admin/catalog/prices/" + priceID.String() + " with provider_links.mobius.plan_id",
		PatchRequired: map[string]map[string]map[string]string{
			"provider_links": {
				"mobius": {
					"plan_id": "<plan id>",
				},
			},
		},
	}
}

func (a *mobiusAdapter) Attach(_ context.Context, link map[string]string, in autoCreateContext) (map[string]string, error) {
	link = normalizeLinkMap(link)
	planID := strings.TrimSpace(link[models.ProcessorKeyPlanID])
	if planID == "" {
		return nil, fmt.Errorf("mobius link requires provider_links.mobius.plan_id")
	}
	provider := "mobius"
	if override := strings.TrimSpace(link[models.ProcessorKeyProvider]); override != "" {
		provider = strings.ToLower(override)
	}

	// NMI plan_ids are operator-chosen AND client-creatable (the id is an input to
	// AddRecurringPlan), so an explicit link is a find-or-CREATE at that id:
	//   - exists: verify it matches the catalog price's immutable money terms
	//     (amount + day-based cycle); a mismatch is a loud error.
	//   - missing: create the plan at the supplied id from the price's terms.
	// When no NMI processor is configured there is no API to verify/create
	// against, so the link is stored as-is (operator-owned).
	if client, ok := a.nmiClient(provider); ok && client != nil {
		detail, err := client.GetRecurringPlanDetailByID(planID)
		if err != nil {
			return nil, fmt.Errorf("verify NMI recurring plan %q: %w", planID, err)
		}
		if detail.Found {
			if in.UnitAmount > 0 && detail.AmountCents != in.UnitAmount {
				return nil, fmt.Errorf("NMI recurring plan %q amount (%d cents) does not match catalog price (%d cents)", planID, detail.AmountCents, in.UnitAmount)
			}
			// day_frequency is only reported for day-based plans; validate it only
			// when NMI returns one (month-based plans report 0 -> unverifiable here).
			if in.BillingCycleDays != nil && *in.BillingCycleDays > 0 && detail.DayFrequency > 0 && detail.DayFrequency != *in.BillingCycleDays {
				return nil, fmt.Errorf("NMI recurring plan %q billing cycle (%d days) does not match catalog price (%d days)", planID, detail.DayFrequency, *in.BillingCycleDays)
			}
		} else {
			if in.RemoteWritesDisabled {
				return nil, fmt.Errorf("NMI recurring plan %q does not exist: %w", planID, errRemoteWritesDisabled)
			}
			if err := a.createPlan(client, planID, in); err != nil {
				return nil, fmt.Errorf("link plan_id %q does not exist and could not be created: %w", planID, err)
			}
		}
	}

	return map[string]string{
		models.ProcessorKeyPlanID:   planID,
		models.ProcessorKeyProvider: provider,
	}, nil
}

// createPlan adds an NMI Recurring Plan at the given plan_id from the price's
// money terms. Shared by AutoCreate (content-addressed id) and Attach (operator
// id). NMI plans are inherently recurring, so a fixed billing frequency and a
// positive amount are required.
func (a *mobiusAdapter) createPlan(client *nmi.NMIClient, planID string, in autoCreateContext) error {
	if in.BillingCycleDays == nil || *in.BillingCycleDays <= 0 {
		return fmt.Errorf("billing_cycle_days is required (NMI plans need a recurring frequency)")
	}
	if in.UnitAmount <= 0 {
		return fmt.Errorf("a positive unit_amount is required")
	}
	planName := ""
	if in.Product != nil {
		planName = strings.TrimSpace(in.Product.DisplayName)
	}
	if planName == "" {
		planName = planID
	}
	// plan_payments=0 means bill forever; OpenRails models open-ended subscriptions.
	return client.AddRecurringPlan(planID, planName, in.UnitAmount, *in.BillingCycleDays, 0)
}

// mobiusDeterministicPlanID is the stable NMI plan_id OpenRails uses for a price.
// It is CONTENT-addressed — derived from the price content key (product slug +
// immutable money terms), NOT the per-DB price UUID — so it is stable across a
// FRESH OpenRails DB: a rebuilt catalog re-derives the same plan_id and
// find-or-attach reattaches to the existing NMI Recurring Plan instead of
// creating a duplicate. Cosmetic edits (display_name/description/providers) do
// not change it. Dots in the content key become hyphens to stay within NMI's
// plan_id charset, e.g. "premium-usd-2300-30".
//
// The plan_id carries NO "openrails-"/tenant/application prefix: the content key
// is the whole id. Operator-supplied (Attach) plan_ids are owned by the operator
// and never renamed by OpenRails, even when this template changes.
func mobiusDeterministicPlanID(productSlug, currency string, unitAmount int64, billingCycleDays *int) string {
	key := openRailsPriceContentKey(productSlug, currency, unitAmount, billingCycleDays)
	return strings.ReplaceAll(key, ".", "-")
}

// nmiClient resolves the NMI client backing the given provider name (defaulting
// to "mobius"). Returns (nil, false) when no NMI processor is configured.
func (a *mobiusAdapter) nmiClient(provider string) (*nmi.NMIClient, bool) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.NMIClients == nil {
		return nil, false
	}
	key := strings.ToLower(strings.TrimSpace(provider))
	if key == "" {
		key = "mobius"
	}
	client, ok := a.svc.rt.NMIClients[key]
	return client, ok
}

// AutoCreate creates (or attaches to) the NMI Recurring Plan for a price under a
// deterministic plan_id. When no NMI processor is configured it returns
// errPendingManualLink so the dispatcher converts the slot to a manual link.
func (a *mobiusAdapter) AutoCreate(_ context.Context, in autoCreateContext) (map[string]string, error) {
	client, ok := a.nmiClient("mobius")
	if !ok || client == nil {
		// No NMI processor configured: defer to manual link.
		return nil, errPendingManualLink
	}
	// NMI recurring plans require a fixed billing frequency.
	if in.BillingCycleDays == nil || *in.BillingCycleDays <= 0 {
		return nil, fmt.Errorf("mobius create-mode requires billing_cycle_days (NMI plans need a recurring frequency)")
	}

	planID := mobiusDeterministicPlanID(in.ProductSlug, in.Currency, in.UnitAmount, in.BillingCycleDays)

	// Find-or-create: prefer an existing plan with this deterministic id.
	found, _, _, err := client.GetRecurringPlanByID(planID)
	if err != nil {
		return nil, fmt.Errorf("lookup recurring plan: %w", err)
	}
	if !found {
		if err := a.createPlan(client, planID, in); err != nil {
			return nil, fmt.Errorf("create recurring plan: %w", err)
		}
	}

	return map[string]string{
		models.ProcessorKeyPlanID:   planID,
		models.ProcessorKeyProvider: "mobius",
	}, nil
}

// Verify performs a live retrieve of the NMI plan and computes plan_name drift.
func (a *mobiusAdapter) Verify(_ context.Context, ids map[string]string, local *priceVerifyContext) ([]DriftField, bool, error) {
	provider := strings.TrimSpace(ids[models.ProcessorKeyProvider])
	client, ok := a.nmiClient(provider)
	if !ok || client == nil {
		// No read API available (NMI not configured): signal sync_disabled.
		return nil, false, nil
	}
	planID := strings.TrimSpace(ids[models.ProcessorKeyPlanID])
	if planID == "" {
		return nil, false, fmt.Errorf("mobius plan_id missing on local processors map")
	}
	found, _, remoteAmountCents, err := client.GetRecurringPlanByID(planID)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, true, nil
	}
	drift := []DriftField{}
	if local != nil {
		if local.UnitAmount != remoteAmountCents {
			drift = append(drift, DriftField{
				Field:          "unit_amount",
				OpenRailsValue: strconv.FormatInt(local.UnitAmount, 10),
				RemoteValue:    strconv.FormatInt(remoteAmountCents, 10),
			})
		}
	}
	return drift, false, nil
}

// Update is a no-op for NMI plans. is_active is not representable on NMI plans,
// and amount/frequency are immutable post-create (parallel to Stripe Price
// financials). With the price display_name removed there are no mutable fields
// left to propagate.
func (a *mobiusAdapter) Update(_ context.Context, _ map[string]string, _ mutableUpdate) error {
	return nil
}
