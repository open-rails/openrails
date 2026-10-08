package subscriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

func TestRenewalIdentityUsesObligationAndAttempt(t *testing.T) {
	mid, psp := uuid.New(), uuid.New()
	boundary := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	p := SubscriptionCollectionPayload{PreviousPeriodEnd: boundary, AcceptedAt: boundary,
		Renewal: RenewalTerms{SubscriptionID: uuid.New(), PeriodStart: boundary, PeriodEnd: boundary.Add(24 * time.Hour), Amount: 1_000_000, Currency: "USD"}}
	id := SubscriptionCollectionOperationID(mid, psp, p)
	changed := p
	changed.AcceptedAt = boundary.Add(time.Minute)
	changed.Initiator = charge.InitiatorCustomer
	changed.PaymentMethodID = uuid.New()
	changed.Renewal.Amount++
	changed.Renewal.PeriodStart = changed.AcceptedAt
	require.Equal(t, id, SubscriptionCollectionOperationID(mid, psp, changed), "changed terms must collide and refuse, not select a fresh charge key")
	changed.PreviousPeriodEnd = boundary.In(time.FixedZone("local", -7*3600))
	require.Equal(t, id, SubscriptionCollectionOperationID(mid, psp, changed), "identity names an instant, not its timezone rendering")
	for name, other := range map[string]uuid.UUID{
		"merchant": SubscriptionCollectionOperationID(uuid.New(), psp, p),
		"psp":      SubscriptionCollectionOperationID(mid, uuid.New(), p),
		"attempt": SubscriptionCollectionOperationID(mid, psp, func() SubscriptionCollectionPayload {
			other := p
			other.Attempt++
			return other
		}()),
	} {
		require.NotEqual(t, id, other, name)
	}
	changed = p
	changed.PreviousPeriodEnd = boundary.Add(24 * time.Hour)
	require.NotEqual(t, id, SubscriptionCollectionOperationID(mid, psp, changed), "next obligation")
}

func TestRenewalTermsBindCoverageWithoutAdmissionNoise(t *testing.T) {
	boundary := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	p := SubscriptionCollectionPayload{PreviousPeriodEnd: boundary, AcceptedAt: boundary,
		Renewal: RenewalTerms{SubscriptionID: uuid.New(), PriceID: uuid.New(), ProductID: uuid.New(), Amount: 1_000_000,
			Currency: "USD", PeriodStart: boundary, PeriodEnd: boundary.Add(24 * time.Hour)}}
	p.OrderReference = ObligationOrderReference(p.Renewal.SubscriptionID, boundary)
	terms, err := NewStripeRenewal(p)
	require.NoError(t, err)
	changed := p
	changed.AcceptedAt = boundary.Add(time.Minute)
	changed.Renewal.ProductName = "renamed display label"
	changed.Renewal.Entitlements = map[string]*int{}
	changed.Renewal.PeriodStart = boundary.In(time.FixedZone("local", 3600))
	unchanged, err := NewStripeRenewal(changed)
	require.NoError(t, err)
	require.Equal(t, terms, unchanged)
	for _, edit := range []func(*SubscriptionCollectionPayload){
		func(p *SubscriptionCollectionPayload) { p.Renewal.PeriodStart = p.Renewal.PeriodStart.Add(time.Minute) },
		func(p *SubscriptionCollectionPayload) { p.Renewal.PeriodEnd = p.Renewal.PeriodEnd.Add(time.Minute) },
		func(p *SubscriptionCollectionPayload) { p.Renewal.Amount++ },
		func(p *SubscriptionCollectionPayload) { p.Renewal.Currency = "EUR" },
		func(p *SubscriptionCollectionPayload) { p.Renewal.Entitlements = map[string]*int{"new_access": nil} },
		func(p *SubscriptionCollectionPayload) { p.Renewal.PriceID = uuid.New() },
	} {
		changed := p
		edit(&changed)
		other, err := NewStripeRenewal(changed)
		require.NoError(t, err)
		require.Equal(t, terms.Obligation, other.Obligation)
		require.NotEqual(t, terms.TermsSHA256, other.TermsSHA256)
	}
}

func TestStripeRenewalTermsCanonicalVector(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	downloads := 3
	p := SubscriptionCollectionPayload{Renewal: RenewalTerms{
		PriceID: uuid.MustParse("10000000-0000-0000-0000-000000000002"), ProductID: uuid.MustParse("10000000-0000-0000-0000-000000000003"),
		Amount: 1_000_000, Currency: "USD", PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour),
		Entitlements: map[string]*int{"view": nil, "download": &downloads},
	}}
	terms, err := NewStripeRenewal(p)
	require.NoError(t, err)
	// This digest is a persisted provider contract, not a Go struct-layout hash.
	require.Equal(t, "0669122397632a04432436a0fd2f360861e7f06021f70e1e2f389c217c913b31", terms.TermsSHA256)
}
