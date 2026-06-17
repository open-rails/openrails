package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
)

// ccbillAdapter implements providerAdapter for CCBill. CCBill does not expose a
// public API for FlexForm creation or read, so:
//   - Attach: validates and stores form_name + flex_id supplied by the operator.
//   - AutoCreate: returns errPendingManualLink (the operator must create the
//     FlexForm in the CCBill control center and PATCH the price with the link).
//   - Verify: returns (nil, false, nil) — no read API → sync_disabled.
//   - Update: no-op.
type ccbillAdapter struct{}

func (a *ccbillAdapter) Name() string { return "ccbill" }

func (a *ccbillAdapter) PendingActionTemplate(priceID uuid.UUID) PendingAction {
	return PendingAction{
		Provider: "ccbill",
		Action:   "create_flexform",
		Hint:     "Create a FlexForm in the CCBill admin portal, then PATCH /merchant/catalog/prices/" + priceID.String() + " with provider_links.ccbill.{form_name,flex_id}",
		PatchRequired: map[string]map[string]map[string]string{
			"provider_links": {
				"ccbill": {
					"form_name": "<form name>",
					"flex_id":   "<flex id>",
				},
			},
		},
	}
}

// Attach stores the operator-supplied form_name + flex_id. CCBill exposes no
// public read API for FlexForms, so OpenRails cannot verify the form exists or
// matches the price's money terms — the ids are accepted as operator-owned
// (the one provider in the shared model without remote link validation).
func (a *ccbillAdapter) Attach(_ context.Context, link map[string]string, _ autoCreateContext) (map[string]string, error) {
	link = normalizeLinkMap(link)
	formName := strings.TrimSpace(link[models.ProcessorKeyCCBillFormName])
	flexID := strings.TrimSpace(link[models.ProcessorKeyCCBillFlexID])
	if formName == "" || flexID == "" {
		return nil, fmt.Errorf("ccbill link requires provider_links.ccbill.form_name and flex_id")
	}
	return map[string]string{
		models.ProcessorKeyCCBillFormName: formName,
		models.ProcessorKeyCCBillFlexID:   flexID,
	}, nil
}

func (a *ccbillAdapter) AutoCreate(_ context.Context, _ autoCreateContext) (map[string]string, error) {
	return nil, errPendingManualLink
}

func (a *ccbillAdapter) Verify(_ context.Context, _ map[string]string, _ *priceVerifyContext) ([]DriftField, bool, error) {
	// CCBill has no read API; report no drift and signal sync_disabled via the
	// dispatcher (it treats nil/nil/nil as sync_disabled).
	return nil, false, nil
}

func (a *ccbillAdapter) Update(_ context.Context, _ map[string]string, _ mutableUpdate) error {
	return nil
}
