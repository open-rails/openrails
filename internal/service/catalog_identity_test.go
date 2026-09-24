package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/stretchr/testify/require"
)

func intPtr(i int) *int       { return &i }
func int64Ptr(i int64) *int64 { return &i }

type priceTerms struct {
	product  uuid.UUID
	amount   int64
	currency string
	access   *int
	renew    bool
	trialAmt *int64
	trialDur *int
}

func (p priceTerms) id() uuid.UUID {
	return priceDeterministicID(p.product, p.amount, p.currency, p.access, p.renew, p.trialAmt, p.trialDur)
}

// #662: the price id is a pure function of the immutable financial tuple
// (the unique_prices_product_amount_window columns), so every column
// participates and equal terms always hash equal.
func TestPriceDeterministicID(t *testing.T) {
	base := priceTerms{uuid.MustParse("11111111-1111-4111-8111-111111111111"), 1_000_000, "usd", intPtr(720), true, int64Ptr(0), intPtr(72)}
	with := func(mut func(*priceTerms)) priceTerms { p := base; mut(&p); return p }
	require.Equal(t, base.id(), base.id())
	require.Equal(t, base.id(), with(func(p *priceTerms) { p.currency = "USD" }).id(), "currency case is canonical")
	for name, changed := range map[string]priceTerms{
		"product":   with(func(p *priceTerms) { p.product = uuid.New() }),
		"amount":    with(func(p *priceTerms) { p.amount = 2_000_000 }),
		"currency":  with(func(p *priceTerms) { p.currency = "eur" }),
		"access":    with(func(p *priceTerms) { p.access = intPtr(1) }),
		"renew":     with(func(p *priceTerms) { p.renew = false }),
		"trial amt": with(func(p *priceTerms) { p.trialAmt = int64Ptr(500) }),
		"trial dur": with(func(p *priceTerms) { p.trialDur = intPtr(24) }),
		// NULLS NOT DISTINCT: an absent value differs from a present zero.
		"nil trial":  with(func(p *priceTerms) { p.trialAmt, p.trialDur = nil, nil }),
		"nil access": with(func(p *priceTerms) { p.access = nil }),
	} {
		require.NotEqual(t, base.id(), changed.id(), name)
	}
	reseeded := priceTerms{base.product, 1_000_000, "usd", intPtr(720), true, nil, nil}
	require.Equal(t, with(func(p *priceTerms) { p.trialAmt, p.trialDur = nil, nil }).id(), reseeded.id(), "equal NULL tuples re-seed stably")
}

// Wipe-resync invariant: content keys derive from product key + money terms,
// never row UUIDs, so a reseeded catalog re-attaches to existing provider
// objects, while a different amount is a different (new) price.
func TestContentKeysSurviveUUIDRegeneration(t *testing.T) {
	now := time.Now().UTC()
	snapshot := func(amount int64) catalog.DriftSnapshot {
		productID := uuid.New()
		return catalog.BuildDriftSnapshot([]*models.Product{{ID: productID, Key: "pro"}},
			[]*models.Price{{ID: uuid.New(), ProductID: productID, Amount: amount, Currency: "usd", AccessDurationHours: intPtr(365 * 24), AutoRenew: true}}, uuid.Nil)
	}
	require.Equal(t, "openrails.pro.usd.10000000.365", internalStripeLookupKey("pro", "USD", 10_000_000, intPtr(365)))
	remote := []catalog.StripePrice{{ID: "price_live", UnitAmount: 1000, Currency: "usd", Active: true, LookupKey: internalStripeLookupKey("pro", "usd", 10_000_000, intPtr(365))}}
	for range 2 { // before and after a wipe: fresh UUIDs, identical terms
		require.Empty(t, catalog.ComputeStripeDrift(nil, remote, snapshot(10_000_000), now))
	}

	repriced := catalog.ComputeStripeDrift(nil, remote, snapshot(29_000_000), now)
	require.Len(t, repriced, 1)
	require.Equal(t, models.CatalogDriftOrphanInStripe, repriced[0].Kind, "a new amount is a new price, not amount drift")
}

// NMI plans are content-addressed; extras archiving relies on recognizing the
// exact shape nmiDeterministicPlanID mints.
func TestNMIPlanIDShape(t *testing.T) {
	minted := nmiDeterministicPlanID("premium", "USD", 23_000_000, intPtr(30))
	require.Equal(t, nmiDeterministicPlanID("premium", "usd", 23_000_000, intPtr(30)), minted)
	require.NotEqual(t, minted, nmiDeterministicPlanID("premium", "usd", 23_000_000, intPtr(365)))
	require.True(t, isContentAddressedNMIPlanID(minted), minted)
	for id, want := range map[string]bool{
		"premium-usd-23000000-30": true, "pro-eur-999-365": true, "vip-gold-usd-999-onetime": true, "a-b-c-usd-1-7": true,
		"legacy-vip-plan": false, "premium-usd-23x0-30": false, "premium-us-2300-30": false, "premium-USD-2300-30": false,
		"-usd-2300-30": false, "usd-2300-30": false, "": false,
	} {
		require.Equal(t, want, isContentAddressedNMIPlanID(id), id)
	}
}

// CUR-8: service entry points accept only registered currencies.
func TestRequireCurrencyConsultsTheRegistry(t *testing.T) {
	for in, want := range map[string]string{"usd": "USD", " USD ": "USD", "EUR": "EUR", "jpy": "JPY"} {
		got, err := requireCurrency(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got)
	}
	for _, bad := range []string{"", "   ", "XYZ", "USDD", "dollars", "acme/tokens", "credit:00000000-0000-0000-0000-000000000000"} {
		_, err := requireCurrency(bad)
		require.Error(t, err, bad)
	}
}

func TestValidateInvokerSpendLimitInputs(t *testing.T) {
	roleID := "22222222-2222-2222-2222-222222222222"
	next, err := ValidateInvokerSpendLimitInputs([]InvokerSpendLimitInput{{
		Scope: " role ", ScopeKey: " " + roleID + " ",
		Windows: []SpendLimitWindowInput{{Key: " day ", WindowSeconds: 86400, Limit: 1000, Currency: " usd "}},
	}})
	require.NoError(t, err)
	require.Equal(t, []InvokerSpendLimitInput{{Scope: "role", ScopeKey: roleID,
		Windows: []SpendLimitWindowInput{{Key: "day", WindowSeconds: 86400, Limit: 1000, Currency: "USD"}}}}, next)

	windows := []SpendLimitWindowInput{{Key: "day", WindowSeconds: 86400, Limit: 1000}}
	_, err = ValidateInvokerSpendLimitInputs([]InvokerSpendLimitInput{
		{Scope: " role ", ScopeKey: " " + roleID + " ", Windows: windows},
		{Scope: "role", ScopeKey: roleID, Windows: windows},
	})
	require.ErrorIs(t, err, ErrInvalidInvokerSpendLimit, "duplicates are detected after normalization")
	require.ErrorContains(t, err, "duplicate delegation for role\x00"+roleID)
}
