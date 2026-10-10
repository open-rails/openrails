package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
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
	billing  *int
	trialAmt *int64
	trialDur *int
}

func (p priceTerms) id() uuid.UUID {
	return priceDeterministicID(p.product, "monthly", p.amount, p.currency, p.access, p.billing, p.trialAmt, p.trialDur)
}

// #662: the price id is a pure function of the immutable financial tuple
// (the prices_product_amount_window_key columns), so every column
// participates and equal terms always hash equal.
func TestPriceDeterministicID(t *testing.T) {
	base := priceTerms{uuid.MustParse("11111111-1111-4111-8111-111111111111"), 1_000_000, "usd", intPtr(720), intPtr(720), int64Ptr(0), intPtr(72)}
	with := func(mut func(*priceTerms)) priceTerms { p := base; mut(&p); return p }
	require.Equal(t, base.id(), base.id())
	require.Equal(t, base.id(), with(func(p *priceTerms) { p.currency = "USD" }).id(), "currency case is canonical")
	for name, changed := range map[string]priceTerms{
		"product":   with(func(p *priceTerms) { p.product = uuid.New() }),
		"amount":    with(func(p *priceTerms) { p.amount = 2_000_000 }),
		"currency":  with(func(p *priceTerms) { p.currency = "eur" }),
		"access":    with(func(p *priceTerms) { p.access = intPtr(1) }),
		"billing":   with(func(p *priceTerms) { p.billing = intPtr(24) }),
		"trial amt": with(func(p *priceTerms) { p.trialAmt = int64Ptr(500) }),
		"trial dur": with(func(p *priceTerms) { p.trialDur = intPtr(24) }),
		// NULLS NOT DISTINCT: an absent value differs from a present zero.
		"nil trial":   with(func(p *priceTerms) { p.trialAmt, p.trialDur = nil, nil }),
		"nil access":  with(func(p *priceTerms) { p.access = nil }),
		"nil billing": with(func(p *priceTerms) { p.billing = nil }),
	} {
		require.NotEqual(t, base.id(), changed.id(), name)
	}
	reseeded := priceTerms{base.product, 1_000_000, "usd", intPtr(720), intPtr(720), nil, nil}
	require.Equal(t, with(func(p *priceTerms) { p.trialAmt, p.trialDur = nil, nil }).id(), reseeded.id(), "equal NULL tuples re-seed stably")
}

// Provider identity follows the retained immutable local ID, not money alone.
// Existing objects remain bound even when they carry older financial markers.
func TestProviderPriceIdentity(t *testing.T) {
	productID := uuid.New()
	base := priceDeterministicID(productID, "monthly", 10_000_000, "USD", intPtr(720), intPtr(720), nil, nil)
	sibling := priceDeterministicID(productID, "special", 10_000_000, "USD", intPtr(720), intPtr(720), nil, nil)
	trial := priceDeterministicID(productID, "monthly", 10_000_000, "USD", intPtr(720), intPtr(720), int64Ptr(0), intPtr(24))
	for _, other := range []uuid.UUID{sibling, trial} {
		require.NotEqual(t, internalStripeLookupKey(base), internalStripeLookupKey(other))
		require.NotEqual(t, nmiDeterministicPlanID(base), nmiDeterministicPlanID(other))
		require.NotEqual(t, solanaPlanID(base, "mint"), solanaPlanID(other, "mint"))
	}
	require.Equal(t, internalStripeLookupKey(base), internalStripeLookupKey(base))
	product := &models.Product{ID: productID, Key: "pro"}
	legacy := &models.Price{ID: uuid.New(), ProductID: productID, Key: "monthly", Amount: 10_000_000, Currency: "USD",
		PSPLinks: map[string]map[string]string{"stripe": {models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_legacy"}}}
	remote := []catalog.StripePrice{{ID: "price_legacy", UnitAmount: 1000, Currency: "usd", Active: true, LookupKey: "openrails.pro.usd.10000000.onetime"}}
	require.Empty(t, catalog.ComputeStripeDrift(nil, remote, catalog.BuildDriftSnapshot([]*models.Product{product}, []*models.Price{legacy}, uuid.Nil), time.Now()))
}

func TestNMIPlanIDShape(t *testing.T) {
	id := uuid.New()
	minted := nmiDeterministicPlanID(id)
	require.Equal(t, nmiDeterministicPlanID(id), minted)
	require.NotEqual(t, minted, nmiDeterministicPlanID(uuid.New()))
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

func TestValidateSpendDelegations(t *testing.T) {
	roleID := "22222222-2222-2222-2222-222222222222"
	next, err := ValidateSpendDelegations([]billing.SpendDelegation{{
		Scope: " role ", ScopeKey: " " + roleID + " ",
		Windows: []billing.BudgetWindow{{Key: " day ", WindowSeconds: 86400, Limit: 1000, Currency: " usd "}},
	}})
	require.NoError(t, err)
	require.Equal(t, []billing.SpendDelegation{{Scope: "role", ScopeKey: roleID,
		Windows: []billing.BudgetWindow{{Key: "day", WindowSeconds: 86400, Limit: 1000, Currency: "USD"}}}}, next)

	windows := []billing.BudgetWindow{{Key: "day", WindowSeconds: 86400, Limit: 1000}}
	_, err = ValidateSpendDelegations([]billing.SpendDelegation{
		{Scope: " role ", ScopeKey: " " + roleID + " ", Windows: windows},
		{Scope: "role", ScopeKey: roleID, Windows: windows},
	})
	require.ErrorIs(t, err, ErrInvalidInvokerSpendLimit, "duplicates are detected after normalization")
	require.ErrorContains(t, err, "duplicate delegation for role\x00"+roleID)
}
