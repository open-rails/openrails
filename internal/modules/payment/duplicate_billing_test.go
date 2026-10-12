package payment

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
)

func TestTierChangeAdmission(t *testing.T) {
	for _, tc := range []struct {
		sub  models.Subscription
		want string
	}{
		{models.Subscription{Status: models.StatusActive, Rail: models.RailStripe}, ""},
		{models.Subscription{Status: models.StatusPastDue, Rail: models.RailStripe}, ""},
		{models.Subscription{Status: models.StatusActive, Rail: models.RailNMI, CollectionPolicy: models.CollectionPolicyEngine}, ""},
		{models.Subscription{Status: models.StatusPending, Rail: models.RailStripe}, codeSubscriptionNotActive},
		{models.Subscription{Status: models.StatusCanceled, Rail: models.RailStripe}, codeSubscriptionNotActive},
	} {
		err := validateTierChangeSubscriptionStatus(&tc.sub)
		if tc.want == "" {
			require.NoError(t, err, "%+v", tc.sub)
			continue
		}
		var tierErr *TierChangeError
		require.ErrorAs(t, err, &tierErr)
		require.Equal(t, tc.want, tierErr.Code)
	}

	ctx := merchantCtx()
	configured, bare := &models.Price{}, &models.Price{}
	configured.SetStripeConfig("price_target")
	mobius := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius"}
	nmiSvc := &PurchaseService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{mobius}}}
	otherPlan := &models.Price{ID: uuid.New(), PSPLinks: map[string]map[string]string{"paykings": {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_paykings"}}}
	for _, tc := range []struct {
		name          string
		svc           *PurchaseService
		sub           *models.Subscription
		current, next *models.Price
		action, want  string
	}{
		{"stripe target configured", &PurchaseService{}, &models.Subscription{Rail: models.RailStripe}, configured, configured, "downgrade", ""},
		{"stripe target missing", &PurchaseService{}, &models.Subscription{Rail: models.RailStripe}, configured, bare, "upgrade", "target price not configured for Stripe"},
		{"ccbill downgrade", &PurchaseService{}, &models.Subscription{Rail: models.RailCCBill}, bare, bare, "downgrade", "downgrades are not supported"},
		{"nmi plan of another provider", nmiSvc, &models.Subscription{Rail: models.RailNMI, PspID: mobius.ID}, bare, otherPlan, "upgrade", "missing NMI plan configuration for payment provider mobius"},
		{"solana target without plan", &PurchaseService{}, &models.Subscription{Rail: models.RailSolana}, bare, bare, "upgrade", "Solana recurring"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.svc.validateTierChangePreviewTarget(ctx, tc.sub, tc.current, tc.next, &UserIdentity{}, tc.action)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// New subscriptions exist only through a saved-method session with explicit
// agreement confirmation; no rail falls back to provider enrollment.
func TestLegacySubscriptionEnrollmentIsClosed(t *testing.T) {
	for _, rail := range []string{"stripe", "nmi", "ccbill", "solana"} {
		_, err := (&PurchaseService{}).processSubscription(context.Background(), nil, nil, nil, nil, nil, rail)
		require.ErrorContains(t, err, "saved-method order attempt and explicit agreement confirmation")
	}
}
