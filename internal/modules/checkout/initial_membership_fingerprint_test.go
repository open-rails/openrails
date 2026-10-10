package checkout

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestInitialMembershipReplayPreservesLegacyFingerprint(t *testing.T) {
	// A quote persisted in the older shape, and the fingerprint stored when
	// the customer accepted this exact offer.
	const legacyTerms = `{
		"collection_policy":"engine",
		"subscription_id":"11111111-1111-4111-8111-111111111111",
		"payment_id":"22222222-2222-4222-8222-222222222222",
		"customer_id":"33333333-3333-4333-8333-333333333333",
		"psp_id":"44444444-4444-4444-8444-444444444444",
		"product_id":"55555555-5555-4555-8555-555555555555",
		"price_id":"66666666-6666-4666-8666-666666666666",
		"payment_method_id":"77777777-7777-4777-8777-777777777777",
		"product_name":"Accepted membership",
		"amount":"10000000","recurring_amount":"10000000","currency":"USD",
		"accepted_at":"2026-10-08T00:00:00Z",
		"period_start":"2026-10-08T00:00:00Z",
		"period_end":"2026-11-07T00:00:00Z",
		"pending":false,"entitlements":{"premium":null}
	}`
	const legacyFingerprint = "1bf3627b89e3f36a9c7daea1ec049b767367c8cf805141c9103cd7e1acbaed73"
	var accepted subscriptions.InitialMembershipTerms
	require.NoError(t, json.Unmarshal([]byte(legacyTerms), &accepted))
	require.Equal(t, new(720), accepted.AccessDurationHours)
	sessionID := uuid.New()
	key := "checkout_attempt:" + sessionID.String()
	payload, err := json.Marshal(map[string]any{
		"checkout_attempt_id": sessionID,
		"terms":               json.RawMessage(legacyTerms),
		"instrument": charge.FrozenInstrument{
			PSPID: accepted.PSPID, Custodian: models.CustodianPSP,
			RailCustomerRef: "vault", RailMethodRef: "card",
		},
		"request_fingerprint": legacyFingerprint, "checkout_idempotency_key": key, "psp": "nmi",
	})
	require.NoError(t, err)
	operation := gen.BillingProviderIntent{
		ID: uuid.New(), MerchantID: uuid.New(), IntentType: subscriptions.TypeInitialMembership,
		Rail: "nmi", PspID: &accepted.PSPID, PriceID: &accepted.PriceID,
		IdempotencyKey: subscriptions.TypeInitialMembership + ":" + key, Payload: payload,
	}
	session := models.CheckoutAttempt{
		ID: sessionID, CustomerID: accepted.CustomerID, PspID: accepted.PSPID, PriceID: &accepted.PriceID,
		Mode: models.CheckoutAttemptModeSubscription, Rail: models.RailNMI,
		Amount: &accepted.Amount, Currency: &accepted.Currency,
		RailState: map[string]any{initialMembershipQuoteKey: legacyTerms},
	}
	stored, err := json.Marshal(session)
	require.NoError(t, err)
	var restored models.CheckoutAttempt
	require.NoError(t, json.Unmarshal(stored, &restored))
	quote, err := readInitialMembershipQuote(&restored)
	require.NoError(t, err)
	fingerprint := initialMembershipQuoteFingerprint(quote)
	require.Equal(t, legacyFingerprint, fingerprint, "a deployment must not strand the accepted operation")
	require.NoError(t, ownsInitialMembership(operation, accepted.CustomerID.String(), accepted.PriceID, fingerprint, &sessionID))
	require.Equal(t, payload, operation.Payload, "replaying must not rewrite the admitted payload or its hash")

	for _, change := range []func(*subscriptions.InitialMembershipTerms){
		func(terms *subscriptions.InitialMembershipTerms) { terms.CancelAfterInitial = true },
		func(terms *subscriptions.InitialMembershipTerms) { terms.AccessDurationHours = nil },
		func(terms *subscriptions.InitialMembershipTerms) { terms.AccessDurationHours = new(72) },
	} {
		changed := quote
		change(&changed)
		fingerprint := initialMembershipQuoteFingerprint(changed)
		require.NotEqual(t, legacyFingerprint, fingerprint)
		require.Error(t, ownsInitialMembership(operation, accepted.CustomerID.String(), accepted.PriceID, fingerprint, &sessionID), "changed renewal or access cannot reuse the accepted order")
	}
	upgrade := quote
	upgrade.Amount = 9_000_000
	upgrade.Replaces = &subscriptions.ReplacedMembership{SubscriptionID: uuid.MustParse("88888888-8888-4888-8888-888888888888"), PriceID: uuid.MustParse("99999999-9999-4999-8999-999999999999"), PeriodEnd: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), Credit: 1_000_000}
	require.NoError(t, upgrade.Validate())
	require.Equal(t, "7a009841f20528dbffdbf5ced20c89d114fa4076f33988e3ba83fba1e1f58b1d", initialMembershipQuoteFingerprint(upgrade), "the retained legacy object precedes replacement terms in the original fingerprint")

}
