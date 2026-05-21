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
//   - Attach: validates and stores plan_id (+ optional provider override).
//   - AutoCreate: deterministic plan_id `openrails-<price_uuid>`; find-or-attach
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

func (a *mobiusAdapter) Attach(ctx context.Context, link map[string]string) (map[string]string, error) {
	link = normalizeLinkMap(link)
	planID := strings.TrimSpace(link[models.ProcessorKeyPlanID])
	if planID == "" {
		return nil, fmt.Errorf("mobius link requires provider_links.mobius.plan_id")
	}
	out := map[string]string{
		models.ProcessorKeyPlanID:   planID,
		models.ProcessorKeyProvider: "mobius",
	}
	if override := strings.TrimSpace(link[models.ProcessorKeyProvider]); override != "" {
		out[models.ProcessorKeyProvider] = strings.ToLower(override)
	}
	return out, nil
}

// mobiusDeterministicPlanID is the stable NMI plan_id OpenRails uses for a
// price. NMI plan_ids are operator-chosen and stable, so the OpenRails price
// UUID is the natural identity.
func mobiusDeterministicPlanID(priceID uuid.UUID) string {
	return "openrails-" + priceID.String()
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

	planID := mobiusDeterministicPlanID(in.PriceID)
	planName := ""
	if in.Product != nil {
		planName = strings.TrimSpace(in.Product.DisplayName)
	}
	if planName == "" {
		planName = planID
	}

	// Find-or-attach: prefer an existing plan with this deterministic id.
	found, _, _, err := client.GetRecurringPlanByID(planID)
	if err != nil {
		return nil, fmt.Errorf("lookup recurring plan: %w", err)
	}
	if !found {
		// plan_payments=0 means bill forever; OpenRails models open-ended
		// subscriptions, so it does not surface a finite payment count here.
		if err := client.AddRecurringPlan(planID, planName, in.UnitAmount, *in.BillingCycleDays, 0); err != nil {
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
