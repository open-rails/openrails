package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
)

// ccbillAdapter implements providerAdapter for CCBill. CCBill does not expose a
// public API for FlexForm creation or read, so:
//   - Attach: validates and stores the flex_id and recurring_billing_option_id
//     supplied by the operator.
//   - AutoCreate: returns errPendingManualLink (the operator must create the
//     FlexForm in the CCBill control center and PATCH the price with the link).
//   - Verify: returns (nil, false, nil) — no read API → sync_disabled.
//   - Update: no-op.
type ccbillAdapter struct{}

func (a *ccbillAdapter) Name() string { return "ccbill" }

func (a *ccbillAdapter) PendingActionTemplate(priceID uuid.UUID) billing.PendingAction {
	return billing.PendingAction{
		PSP:    "ccbill",
		Action: "create_flexform",
		Hint:   "Create a FlexForm in the CCBill admin portal, then PATCH /catalog/prices/" + priceID.String() + " with psps.ccbill.{flex_id,recurring_billing_option_id}",
		PatchRequired: map[string]map[string]map[string]string{
			"psps": {
				"ccbill": {
					"flex_id":                     "<flex id>",
					"recurring_billing_option_id": "<recurring billing option id>",
				},
			},
		},
	}
}

// Attach stores the operator-supplied CCBill settings: a FlexForm (flex_id;
// the hosted purchase page) and/or a Recurring Billing Option
// (recurring_billing_option_id; the price identity). Archived tiers may carry
// the RBO alone. CCBill has no read API, so the ids are not validated.
func (a *ccbillAdapter) Attach(_ context.Context, link map[string]string, _ autoCreateContext) (map[string]string, error) {
	link = normalizeLinkMap(link)
	if err := catalogReferenceKeys(link, models.RailKeyCCBillFlexID, models.RailKeyCCBillRecurringBillingOption); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, key := range []string{models.RailKeyCCBillFlexID, models.RailKeyCCBillRecurringBillingOption} {
		if value := strings.TrimSpace(link[key]); value != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ccbill settings require a flex_id and/or a recurring_billing_option_id")
	}
	return out, nil
}

func (a *ccbillAdapter) AutoCreate(_ context.Context, _ autoCreateContext) (map[string]string, error) {
	return nil, errPendingManualLink
}

// Verify cannot read CCBill, which has no catalog read API: the link is
// sync_disabled, never in_sync.
func (a *ccbillAdapter) Verify(_ context.Context, _ map[string]string, _ *priceVerifyContext) ([]billing.DriftField, bool, error) {
	return nil, false, fmt.Errorf("ccbill has no catalog read API: %w", errSyncDisabled)
}

func (a *ccbillAdapter) Update(_ context.Context, _ map[string]string, _ mutableUpdate) error {
	return nil
}
