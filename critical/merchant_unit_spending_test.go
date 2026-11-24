//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// A merchant's own units are bought with money at each pack's own rate, given
// as plan allowances and gifts, and spent on usage and on products priced in
// them; every lot keeps its units, what was paid and in what currency.
func TestMerchantUnits(t *testing.T) {
	t.Run("OR-J48-S01/packs-buy-units-at-their-own-rates", unitPacks)
	t.Run("OR-J48-S02/the-same-units-bought-in-another-currency", unitPackInEUR)
	t.Run("OR-J48-S03/a-plan-allowance-and-a-gift-give-units", unitAllowanceAndGift)
	t.Run("OR-J48-S04/a-unit-priced-product-is-paid-from-the-balance", unitPricedProduct)
	t.Run("OR-J48-S05/usage-priced-in-units-draws-lots-soonest-expiry-first", unitUsage)
	t.Run("OR-J48-S06/an-unspent-pack-expires-in-its-unit", unitExpiry)
	t.Run("OR-J48-S07/a-refund-claws-back-its-lots-units-first", unitRefundByLot)
}

// unitPaymentWorld declares GOLD and CREDITS on alpha, whose card PSP is
// Stripe; back starts the clock that long ago, so a test can advance it.
func unitPaymentWorld(t *testing.T, back time.Duration) (*unitShop, *providerLedger) {
	t.Helper()
	a, p := newPaymentWorld(t, func(w *world) {
		w.clock.Advance(-back)
		alpha := cfgMerchant(w.cfg, "alpha")
		alpha.Currencies = []billing.MerchantCurrency{goldUnit, creditsUnit}
		setCfgMerchant(&w.cfg, "alpha", alpha)
	})
	return &unitShop{a: a, p: p, alice: billing.CustomerID(uuid.MustParse(a.alice.ID))}, p
}

type unitShop struct {
	a     *authorityWorld
	p     *providerLedger
	alice billing.CustomerID
}

type unitPrice struct {
	key      string
	amount   int64
	currency catalog.Currency
}

// pack is a product granting units of unit for each of prices.
func (s *unitShop) pack(t *testing.T, key string, unit catalog.Currency, units int64, days int, prices ...unitPrice) map[string]billing.PriceID {
	t.Helper()
	product, err := s.a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Ownership: catalog.OwnershipConsumable,
		CreditGrant: &catalog.CreditGrantSpec{Currency: unit, Amount: ptr(units), ExpiresAfterDays: ptr(days)}})
	require.NoError(t, err)
	out := map[string]billing.PriceID{}
	for _, p := range prices {
		price, err := s.a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: p.key, UnitAmount: p.amount, Currency: p.currency})
		require.NoError(t, err)
		out[p.key] = price.ID
	}
	return out
}

// item is a product with keys, priced in a unit.
func (s *unitShop) item(t *testing.T, key string, ownership catalog.Ownership, amount int64, unit catalog.Currency, keys ...string) billing.PriceID {
	t.Helper()
	product, err := s.a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Ownership: ownership, Entitlements: keys})
	require.NoError(t, err)
	price, err := s.a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "price", UnitAmount: amount, Currency: unit})
	require.NoError(t, err)
	return price.ID
}

// buy pays price with a card; it answers the completed order.
func (s *unitShop) buy(t *testing.T, price billing.PriceID, total int64, key string) billing.Order {
	t.Helper()
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}, ExpectedTotal: ptr(total), Reusable: ptr(true),
		Payment: &billing.OrderPaymentParams{Token: s.p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := s.a.request(t, http.MethodPost, "/v1/me/orders", s.a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	require.NotNil(t, order.Payment.PaymentID)
	return order
}

// spend orders price, which is in a unit, with no payment: the balance pays.
func (s *unitShop) spend(t *testing.T, price billing.PriceID, key string) (int, []byte) {
	t.Helper()
	return s.a.request(t, http.MethodPost, "/v1/me/orders", s.a.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}},
		map[string]string{"Idempotency-Key": key})
}

func unitBalance(t *testing.T, client *openrails.Client, customer billing.CustomerID, unit catalog.Currency, available, held, posted int64) *billing.Balance {
	t.Helper()
	b, err := client.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: unit})
	require.NoError(t, err)
	require.Equal(t, unit, b.Currency)
	require.Equal(t, available, b.AvailableAmount, "available %s", unit)
	require.Equal(t, held, b.HeldAmount, "held %s", unit)
	require.Equal(t, posted, b.BalanceAmount, "posted %s", unit)
	require.Zero(t, b.OwedAmount, "nothing is owed in a unit")
	return b
}

// lots are the customer's lots in unit, oldest first.
func lots(t *testing.T, client *openrails.Client, customer billing.CustomerID, unit catalog.Currency) []billing.CreditGrant {
	t.Helper()
	page, err := client.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer, Currency: unit})
	require.NoError(t, err)
	out := make([]billing.CreditGrant, 0, len(page.Items))
	for i := len(page.Items) - 1; i >= 0; i-- {
		out = append(out, page.Items[i])
	}
	return out
}

type unitLeg struct {
	Type, Debit, Credit, Source, SourceID string
	Amount                                int64
	Lot, Order                            *uuid.UUID
}

// unitLegs are the merchant's ledger transfers in unit, in posting order.
func unitLegs(t *testing.T, a *authorityWorld, unit catalog.Currency) []unitLeg {
	t.Helper()
	transfers, accounts := pgx.Identifier{a.schema, "ledger_transfers"}.Sanitize(), pgx.Identifier{a.schema, "ledger_accounts"}.Sanitize()
	rows, err := a.pool.Query(t.Context(), "SELECT t.transfer_type, d.account_type, c.account_type, t.source, t.source_id, t.amount, t.credit_grant_id, t.order_id FROM "+transfers+" t"+
		" JOIN "+accounts+" d ON d.merchant_id = t.merchant_id AND d.id = t.debit_account_id JOIN "+accounts+" c ON c.merchant_id = t.merchant_id AND c.id = t.credit_account_id"+
		" WHERE t.merchant_id = $1 AND t.currency = $2 ORDER BY t.created_at, t.id", a.alpha.MerchantID().UUID(), string(unit))
	require.NoError(t, err)
	defer rows.Close()
	var out []unitLeg
	for rows.Next() {
		var leg unitLeg
		require.NoError(t, rows.Scan(&leg.Type, &leg.Debit, &leg.Credit, &leg.Source, &leg.SourceID, &leg.Amount, &leg.Lot, &leg.Order))
		out = append(out, leg)
	}
	require.NoError(t, rows.Err())
	return out
}

// systemBalance is the credits less debits of the merchant's account of type
// in unit, zero without one.
func systemBalance(t *testing.T, a *authorityWorld, unit catalog.Currency, accountType ledger.AccountType) int64 {
	t.Helper()
	var balance int64
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(credits_posted - debits_posted), 0)::bigint FROM "+pgx.Identifier{a.schema, "ledger_accounts"}.Sanitize()+
		" WHERE merchant_id = $1 AND currency = $2 AND account_type = $3 AND customer_id IS NULL", a.alpha.MerchantID().UUID(), string(unit), string(accountType)).Scan(&balance))
	return balance
}

func unitPacks(t *testing.T) {
	s, p := unitPaymentWorld(t, 0)
	a := s.a
	gold := s.pack(t, "gold-400", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"})
	small := s.pack(t, "credits-20", "CREDITS", 2000, 365, unitPrice{"usd", 20_000_000, "USD"})
	bonus := s.pack(t, "credits-55", "CREDITS", 5500, 365, unitPrice{"usd", 50_000_000, "USD"})
	s.buy(t, gold["usd"], 20_000_000, "buy-gold")
	s.buy(t, small["usd"], 20_000_000, "buy-20-credits")
	s.buy(t, bonus["usd"], 50_000_000, "buy-55-credits")
	charged := []int64{}
	for _, effect := range p.effects(t) {
		require.Equal(t, "usd", effect.Currency)
		charged = append(charged, effect.Amount)
	}
	require.ElementsMatch(t, []int64{2000, 2000, 5000}, charged, "the cards paid dollars; no provider sees a unit")

	b := unitBalance(t, a.alpha, s.alice, "GOLD", 400, 0, 400)
	require.Equal(t, &billing.MerchantCurrency{Code: "GOLD", Name: "Gold coins", Symbol: "🪙", Scale: 0}, b.Unit, "a unit's balance names its scale")
	b = unitBalance(t, a.alpha, s.alice, "CREDITS", 7500, 0, 7500)
	require.Equal(t, 2, b.Unit.Scale, "75.00 credits")
	usd, err := a.alpha.GetBalance(t.Context(), s.alice, billing.GetBalanceParams{Currency: "USD"})
	require.NoError(t, err)
	require.Zero(t, usd.BalanceAmount, "buying units credits no dollars")
	require.Nil(t, usd.Unit)

	goldLots := lots(t, a.alpha, s.alice, "GOLD")
	require.Len(t, goldLots, 1)
	require.EqualValues(t, 400, goldLots[0].Amount)
	require.Equal(t, billing.CreditFundingPurchase, goldLots[0].FundingSource)
	require.EqualValues(t, 20_000_000, *goldLots[0].PaidAmount)
	require.Equal(t, catalog.Currency("USD"), *goldLots[0].PaidCurrency)
	creditLots := lots(t, a.alpha, s.alice, "CREDITS")
	require.Len(t, creditLots, 2)
	require.EqualValues(t, 2000, creditLots[0].Amount)
	require.EqualValues(t, 20_000_000, *creditLots[0].PaidAmount)
	require.EqualValues(t, 5500, creditLots[1].Amount, "the $50 pack's bonus is part of its lot: its rate is $0.909 a credit")
	require.EqualValues(t, 50_000_000, *creditLots[1].PaidAmount)
	require.Equal(t, catalog.Currency("USD"), *creditLots[1].PaidCurrency)

	// Each lot is one deposit of all its units from processor_clearing: no
	// bonus from promotional funding, no cash in the unit.
	for unit, want := range map[catalog.Currency][]int64{"GOLD": {400}, "CREDITS": {2000, 5500}} {
		var deposits []int64
		for _, leg := range unitLegs(t, a, unit) {
			require.Equal(t, "deposit", leg.Type, "%+v", leg)
			require.Equal(t, string(ledger.RailClearing), leg.Debit)
			require.Equal(t, string(ledger.CustomerBalance), leg.Credit)
			require.NotNil(t, leg.Lot)
			deposits = append(deposits, leg.Amount)
		}
		require.Equal(t, want, deposits, unit)
	}

	status, raw := a.request(t, http.MethodGet, "/v1/me", a.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var me billing.CustomerAccount
	require.NoError(t, json.Unmarshal(raw, &me))
	units := map[catalog.Currency]*billing.MerchantCurrency{}
	for _, b := range me.Balances {
		units[b.Currency] = b.Unit
	}
	require.Equal(t, "Gold coins", units["GOLD"].Name, "the customer's balances name their units")
	require.Equal(t, 2, units["CREDITS"].Scale)
	status, raw = a.request(t, http.MethodGet, "/v1/me/balance/transactions?currency=CREDITS", a.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var statement billing.ListPage[billing.BalanceTransaction]
	require.NoError(t, json.Unmarshal(raw, &statement))
	require.Len(t, statement.Items, 2, "the statement lists each pack's deposit")
}

func unitPackInEUR(t *testing.T) {
	s, p := unitPaymentWorld(t, 0)
	a := s.a
	gold := s.pack(t, "gold-400", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"}, unitPrice{"eur", 18_000_000, "EUR"})
	preview, err := a.alpha.PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: s.alice, Lines: []billing.OrderLineParams{{PriceID: gold["eur"]}}})
	require.NoError(t, err)
	require.Equal(t, catalog.Currency("EUR"), preview.Currency)
	require.NotEmpty(t, preview.PaymentOptions, "a card pays the euro price")
	s.buy(t, gold["usd"], 20_000_000, "gold-in-dollars")
	s.buy(t, gold["eur"], 18_000_000, "gold-in-euros")
	currencies := []string{}
	for _, effect := range p.effects(t) {
		currencies = append(currencies, effect.Currency)
	}
	require.ElementsMatch(t, []string{"usd", "eur"}, currencies)
	unitBalance(t, a.alpha, s.alice, "GOLD", 800, 0, 800)
	goldLots := lots(t, a.alpha, s.alice, "GOLD")
	require.Len(t, goldLots, 2, "one GOLD balance, whatever currency bought it")
	require.Equal(t, catalog.Currency("USD"), *goldLots[0].PaidCurrency)
	require.Equal(t, catalog.Currency("EUR"), *goldLots[1].PaidCurrency)
	require.EqualValues(t, 18_000_000, *goldLots[1].PaidAmount)
	require.EqualValues(t, 400, goldLots[1].Amount)
	for _, currency := range []catalog.Currency{"USD", "EUR"} {
		b, err := a.alpha.GetBalance(t.Context(), s.alice, billing.GetBalanceParams{Currency: currency})
		require.NoError(t, err)
		require.Zero(t, b.BalanceAmount, "nothing converts: the payments credit no %s", currency)
	}
}

func unitAllowanceAndGift(t *testing.T) {
	s, _ := unitPaymentWorld(t, 800*24*time.Hour)
	a := s.a
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "pro", DisplayName: "Pro", Entitlements: []string{"pro:member"},
		CreditGrant: &catalog.CreditGrantSpec{Currency: "GOLD", Amount: ptr(int64(100)), ExpiresAfterDays: ptr(90)}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(720)})
	require.NoError(t, err)
	order := s.buy(t, price.ID, 10_000_000, "pro-plan")
	require.NotNil(t, order.Lines[0].SubscriptionID)
	access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: s.alice, Entitlements: []string{"pro:member"}})
	require.NoError(t, err)
	require.True(t, access.Entitlements["pro:member"])
	unitBalance(t, a.alpha, s.alice, "GOLD", 100, 0, 100)

	sub, err := a.alpha.GetSubscription(t.Context(), *order.Lines[0].SubscriptionID)
	require.NoError(t, err)
	due := *sub.CurrentPeriodEndsAt
	a.clock.Advance(due.Sub(a.clock.Now()))
	fleet := startFleet(t, a.world)
	runRegistered(t, fleet, "openrails.billing_pass", 2)
	require.Eventually(t, func() bool {
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		return err == nil && current.CurrentPeriodStartsAt != nil && current.CurrentPeriodStartsAt.Equal(due)
	}, 20*time.Second, 20*time.Millisecond)
	unitBalance(t, a.alpha, s.alice, "GOLD", 200, 0, 200)

	gift, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.alice, Currency: "GOLD", Amount: 25, Source: "critical", SourceID: "goodwill",
		FundingSource: billing.CreditGrantFundingPromotion}})
	require.NoError(t, err)
	require.Nil(t, gift[0].PaidAmount, "a gift is paid for by nobody")
	unitBalance(t, a.alpha, s.alice, "GOLD", 225, 0, 225)

	goldLots := lots(t, a.alpha, s.alice, "GOLD")
	require.Len(t, goldLots, 3)
	for _, lot := range goldLots[:2] {
		require.Equal(t, billing.CreditFundingRecurring, lot.FundingSource, "each paid cycle delivers its allowance")
		require.EqualValues(t, 100, lot.Amount)
		require.EqualValues(t, 10_000_000, *lot.PaidAmount)
		require.Equal(t, catalog.Currency("USD"), *lot.PaidCurrency)
		require.Equal(t, lot.StartsAt.AddDate(0, 0, 90), *lot.ExpiresAt)
	}
	require.Equal(t, billing.CreditFundingPromotion, goldLots[2].FundingSource)
	legs := unitLegs(t, a, "GOLD")
	require.Len(t, legs, 3)
	require.Equal(t, string(ledger.RailClearing), legs[0].Debit, "a bought allowance is sold")
	require.Equal(t, string(ledger.RailClearing), legs[1].Debit)
	require.Equal(t, string(ledger.PromotionalFunding), legs[2].Debit, "a gift of units is granted, not sold")
	require.Equal(t, int64(-200), systemBalance(t, a, "GOLD", ledger.RailClearing))
	require.Equal(t, int64(-25), systemBalance(t, a, "GOLD", ledger.PromotionalFunding))
}

func unitPricedProduct(t *testing.T) {
	s, _ := unitPaymentWorld(t, 0)
	a := s.a
	gold := s.pack(t, "gold-400", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"})
	sword := s.item(t, "sword", catalog.OwnershipUnique, 50, "GOLD", "shop:sword")
	chest := s.item(t, "chest", catalog.OwnershipConsumable, 1000, "GOLD", "shop:chest")

	preview, err := a.alpha.PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: s.alice, Lines: []billing.OrderLineParams{{PriceID: sword}}})
	require.NoError(t, err)
	require.Equal(t, catalog.Currency("GOLD"), preview.Currency)
	require.EqualValues(t, 50, preview.Total)
	require.Empty(t, preview.PaymentOptions, "no provider takes a unit")

	status, raw := s.spend(t, sword, "sword-unfunded")
	require.Equal(t, http.StatusPaymentRequired, status, string(raw))
	require.Contains(t, string(raw), string(billing.CodeInsufficientCredits))
	orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: s.alice})
	require.NoError(t, err)
	require.Empty(t, orders.Items, "a refused order is not created")

	s.buy(t, gold["usd"], 20_000_000, "buy-gold")
	status, raw = s.spend(t, sword, "sword")
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status, "the balance paid it as it was created")
	require.Equal(t, billing.OrderPaymentSucceeded, order.Payment.Status)
	require.Nil(t, order.Payment.PaymentID, "no payment moved money")
	require.Equal(t, catalog.Currency("GOLD"), order.Currency)
	require.EqualValues(t, 50, order.Total)
	require.NotNil(t, order.Number)
	access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: s.alice, Entitlements: []string{"shop:sword"}})
	require.NoError(t, err)
	require.True(t, access.Entitlements["shop:sword"], "the product's key is held")
	unitBalance(t, a.alpha, s.alice, "GOLD", 350, 0, 350)

	var spends []unitLeg
	for _, leg := range unitLegs(t, a, "GOLD") {
		if leg.Type == "credit_spend" {
			spends = append(spends, leg)
		}
	}
	require.Len(t, spends, 1)
	require.Equal(t, string(ledger.CustomerBalance), spends[0].Debit)
	require.Equal(t, string(ledger.PlatformRevenue), spends[0].Credit)
	require.EqualValues(t, 50, spends[0].Amount)
	require.Equal(t, order.ID.UUID(), *spends[0].Order, "the spend names its order")
	require.Equal(t, "order_line", spends[0].Source)
	require.Equal(t, order.Lines[0].ID.String(), spends[0].SourceID, "and its line")

	status, raw = s.spend(t, sword, "sword-again")
	require.Equal(t, http.StatusConflict, status, string(raw))
	require.Contains(t, string(raw), "already_owned")
	status, raw = s.spend(t, chest, "chest")
	require.Equal(t, http.StatusPaymentRequired, status, string(raw))
	unitBalance(t, a.alpha, s.alice, "GOLD", 350, 0, 350)

	// No card takes an order in a unit.
	potion := s.item(t, "potion", catalog.OwnershipConsumable, 5, "GOLD")
	status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: potion}}, ExpectedTotal: ptr(int64(5)),
		Payment: &billing.OrderPaymentParams{Token: s.p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}, map[string]string{"Idempotency-Key": "potion-by-card"})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	require.Contains(t, string(raw), `"param":"payment"`)
	// The merchant's backend buys in units for the customer.
	bought, err := a.alpha.CreateOrder(t.Context(), "potion-by-backend", billing.CreateOrderParams{CustomerID: s.alice, Lines: []billing.OrderLineParams{{PriceID: potion}}})
	require.NoError(t, err)
	require.Equal(t, billing.OrderComplete, bought.Status)
	unitBalance(t, a.alpha, s.alice, "GOLD", 345, 0, 345)

	// A unit price is a one-off the balance pays: it bills no cycle and
	// buys no credit.
	_, err = a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: mustProduct(t, a, "sword"), Key: "monthly", UnitAmount: 10, Currency: "GOLD", BillingIntervalHours: ptr(720)})
	requireCode(t, err, billing.CodeInvalidParam)
	_, err = a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: mustProduct(t, a, "gold-400"), Key: "in-gold", UnitAmount: 10, Currency: "GOLD"})
	requireCode(t, err, billing.CodeInvalidParam)
}

func mustProduct(t *testing.T, a *authorityWorld, key string) billing.ProductID {
	t.Helper()
	products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{key}})
	require.NoError(t, err)
	require.Len(t, products.Items, 1)
	return products.Items[0].ID
}

func unitUsage(t *testing.T) {
	s, _ := unitPaymentWorld(t, 0)
	a := s.a
	images, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "images", DisplayName: "Images"})
	require.NoError(t, err)
	_, err = a.alpha.SetMeter(t.Context(), "images", billing.SetMeterParams{EventType: "image", ValueProperty: "images", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: images.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "GOLD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 4}}})})
	require.NoError(t, err, "4 GOLD an image")
	tokens, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "tokens", DisplayName: "Tokens"})
	require.NoError(t, err)
	_, err = a.alpha.SetMeter(t.Context(), "tokens", billing.SetMeterParams{EventType: "completion", ValueProperty: "tokens", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: tokens.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "CREDITS", PerUnit: &catalog.PerUnitPrice{UnitAmount: 100, DivideBy: 1000}}})})
	require.NoError(t, err, "1 credit per 1k tokens")
	_, err = a.alpha.SetMeter(t.Context(), "gems", billing.SetMeterParams{EventType: "gem", ValueProperty: "n", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: images.ID, Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "GEMS"}})})
	requireCode(t, err, billing.CodeCurrencyUnsupported)

	customer := billing.CustomerID(uuid.New())
	_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
		{CustomerID: customer, Currency: "GOLD", Amount: 100, Source: "critical", SourceID: "lasting"},
		{CustomerID: customer, Currency: "GOLD", Amount: 10, Source: "critical", SourceID: "soon", ExpiresAt: ptr(a.clock.Now().Add(48 * time.Hour))},
		{CustomerID: customer, Currency: "CREDITS", Amount: 500, Source: "critical", SourceID: "credits"},
	})
	require.NoError(t, err)
	admit := func(id, event string, dims map[string]int64) billing.AdmissionVerdict {
		return admitOne(t, a.alpha, billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
			Currency: map[string]catalog.Currency{"image": "GOLD", "completion": "CREDITS"}[event], ExpiresAt: ptr(a.clock.Now().Add(time.Hour)),
			Usage: &billing.UsageEstimate{EventType: event, Dimensions: dims}})
	}
	verdict := admit("three-images", "image", map[string]int64{"images": 3})
	require.True(t, verdict.Allowed(), "%+v", verdict)
	require.EqualValues(t, 12, verdict.Admission.EstimatedAmount)
	unitBalance(t, a.alpha, customer, "GOLD", 98, 12, 110)
	receipt := captureUsage(t, a.alpha, "three-images", "image", map[string]int64{"images": 3}, nil)
	require.Equal(t, catalog.Currency("GOLD"), receipt.Currency)
	verdict = admit("tokens", "completion", map[string]int64{"tokens": 2500})
	require.True(t, verdict.Allowed(), "%+v", verdict)
	require.EqualValues(t, 250, verdict.Admission.EstimatedAmount, "2.50 credits for 2,500 tokens")
	captureUsage(t, a.alpha, "tokens", "completion", map[string]int64{"tokens": 2500}, nil)

	// A top-up posts the usage first: the lot expiring soonest pays.
	_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
		{CustomerID: customer, Currency: "GOLD", Amount: 1, Source: "critical", SourceID: "post-gold"},
		{CustomerID: customer, Currency: "CREDITS", Amount: 1, Source: "critical", SourceID: "post-credits"},
	})
	require.NoError(t, err)
	unitBalance(t, a.alpha, customer, "GOLD", 99, 0, 99)
	unitBalance(t, a.alpha, customer, "CREDITS", 251, 0, 251)
	drawn := map[int64]int64{}
	for _, lot := range lots(t, a.alpha, customer, "GOLD") {
		drawn[lot.Amount] = lot.SpentAmount
	}
	require.Equal(t, map[int64]int64{100: 2, 10: 10, 1: 0}, drawn, "soonest expiry first, one spend per lot")

	// Admitted usage the units cannot cover takes what they hold and leaves
	// nothing owed: a unit is never owed.
	thin := billing.CustomerID(uuid.New())
	_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: thin, Currency: "GOLD", Amount: 10, Source: "critical", SourceID: "thin"}})
	require.NoError(t, err)
	verdict = admitOne(t, a.alpha, billing.AdmitParams{RequestID: "over", CustomerID: thin, Invoker: thin.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "GOLD",
		ExpiresAt: ptr(a.clock.Now().Add(time.Hour)), Usage: &billing.UsageEstimate{EventType: "image", Dimensions: map[string]int64{"images": 2}}})
	require.True(t, verdict.Allowed())
	captureUsage(t, a.alpha, "over", "image", map[string]int64{"images": 4}, nil)
	_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: thin, Currency: "GOLD", Amount: 5, Source: "critical", SourceID: "after"}})
	require.NoError(t, err)
	unitBalance(t, a.alpha, thin, "GOLD", 5, 0, 5)
	var arrears int
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "ledger_accounts"}.Sanitize()+
		" WHERE merchant_id = $1 AND account_type = $2 AND currency IN ('GOLD', 'CREDITS')", a.alpha.MerchantID().UUID(), string(ledger.ArrearsLiability)).Scan(&arrears))
	require.Zero(t, arrears, "no arrears account exists in a unit")
	requireDenied(t, admitOne(t, a.alpha, billing.AdmitParams{RequestID: "beyond", CustomerID: thin, Invoker: thin.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "GOLD",
		ExpiresAt: ptr(a.clock.Now().Add(time.Hour)), Usage: &billing.UsageEstimate{EventType: "image", Dimensions: map[string]int64{"images": 2}}}), "insufficient_balance")
}

func unitExpiry(t *testing.T) {
	s, _ := unitPaymentWorld(t, 800*24*time.Hour)
	a := s.a
	gold := s.pack(t, "gold-monthly", "GOLD", 400, 30, unitPrice{"usd", 20_000_000, "USD"})
	potion := s.item(t, "potion", catalog.OwnershipConsumable, 50, "GOLD")
	s.buy(t, gold["usd"], 20_000_000, "buy-gold")
	status, raw := s.spend(t, potion, "potion")
	require.Equal(t, http.StatusCreated, status, string(raw))
	lot := lots(t, a.alpha, s.alice, "GOLD")[0]
	require.Equal(t, lot.StartsAt.AddDate(0, 0, 30), *lot.ExpiresAt)

	a.clock.Advance(lot.ExpiresAt.Sub(a.clock.Now()))
	status, raw = s.spend(t, potion, "potion-at-expiry")
	require.Equal(t, http.StatusPaymentRequired, status, "a lapsed lot is never drawn: %s", raw)
	fleet := startFleet(t, a.world)
	runRegistered(t, fleet, "openrails.billing_pass", 1)
	unitBalance(t, a.alpha, s.alice, "GOLD", 0, 0, 0)
	lot = lots(t, a.alpha, s.alice, "GOLD")[0]
	require.Equal(t, billing.CreditGrantExpired, lot.State)
	require.EqualValues(t, 350, lot.ExpiredAmount)
	require.EqualValues(t, 50, lot.SpentAmount)
	var expired []unitLeg
	for _, leg := range unitLegs(t, a, "GOLD") {
		if leg.Type == "credit_expire" {
			expired = append(expired, leg)
		}
	}
	require.Len(t, expired, 1)
	require.Equal(t, string(ledger.ExpiredCredits), expired[0].Credit)
	require.EqualValues(t, 350, expired[0].Amount, "what was left expires in the unit, once")
}

func unitRefundByLot(t *testing.T) {
	s, p := unitPaymentWorld(t, 0)
	a := s.a
	gold := s.pack(t, "gold-400", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"})
	potion := s.item(t, "potion", catalog.OwnershipConsumable, 100, "GOLD")
	order := s.buy(t, gold["usd"], 20_000_000, "buy-gold")
	payment := *order.Payment.PaymentID
	status, raw := s.spend(t, potion, "potion")
	require.Equal(t, http.StatusCreated, status, string(raw))
	unitBalance(t, a.alpha, s.alice, "GOLD", 300, 0, 300)

	// $16 back would take 320 GOLD; 100 of the 400 are spent.
	_, err := a.alpha.RefundPayment(t.Context(), "too-much", payment, billing.RefundPaymentParams{Amount: 16_000_000, Reason: "Too much"})
	require.Error(t, err, "the refund needs its share of the lot unspent")
	unitBalance(t, a.alpha, s.alice, "GOLD", 300, 0, 300)
	for _, effect := range p.effects(t) {
		require.NotEqual(t, "refund", effect.Kind, "no money went back")
	}

	refund, err := a.alpha.RefundPayment(t.Context(), "a-quarter", payment, billing.RefundPaymentParams{Amount: 5_000_000, Reason: "A quarter back"})
	require.NoError(t, err)
	require.Equal(t, billing.PaymentSucceeded, refund.Status)
	require.EqualValues(t, -5_000_000, refund.Amount, "the refund is dollars")
	unitBalance(t, a.alpha, s.alice, "GOLD", 200, 0, 200)
	lot := lots(t, a.alpha, s.alice, "GOLD")[0]
	require.EqualValues(t, 200, lot.RemainingAmount, "a quarter of the price takes a quarter of the units, from those left")

	// A chargeback of the other $15 takes the 200 left and books the 100
	// spent as consumed; winning the dispute returns them.
	var charge, intent string
	for _, effect := range p.effects(t) {
		if effect.Kind == "charge" {
			charge, intent = effect.ID, effect.Operation
		}
	}
	require.NotEmpty(t, charge)
	notice := func(id, kind string, object stripemock.Object) {
		body := stripeNotice(t, id, kind, object)
		status, raw := callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
		require.Equal(t, http.StatusOK, status, raw)
	}
	dispute := stripemock.Object{"id": "dp_gold", "object": "dispute", "amount": 1500, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
	notice("evt_gold_dispute", "charge.dispute.created", dispute)
	unitBalance(t, a.alpha, s.alice, "GOLD", 0, 0, 0)
	lot = lots(t, a.alpha, s.alice, "GOLD")[0]
	require.Zero(t, lot.RemainingAmount)
	sources := map[string]int64{}
	for _, leg := range unitLegs(t, a, "GOLD") {
		if leg.Lot != nil && *leg.Lot == lot.ID.UUID() && leg.Type != "deposit" && leg.Type != "credit_spend" {
			sources[leg.Source] += leg.Amount
			require.NotEqual(t, "credit_refund_cash", leg.Type, "a unit has no cash leg")
		}
	}
	require.Equal(t, map[string]int64{"credit": 300, "consumed_credit": 100, "units_returned": 400}, sources)

	dispute["status"] = "won"
	notice("evt_gold_dispute_won", "charge.dispute.closed", dispute)
	unitBalance(t, a.alpha, s.alice, "GOLD", 200, 0, 200)
	lot = lots(t, a.alpha, s.alice, "GOLD")[0]
	require.EqualValues(t, 200, lot.RemainingAmount, "the won dispute returns what the chargeback took, not the refund's units")
	require.Zero(t, systemBalance(t, a, "GOLD", ledger.CreditRefundClearing), "the clearing account nets to zero")
	require.EqualValues(t, 0, systemBalance(t, a, "GOLD", ledger.CreditRefundLoss), "the consumed units are no loss once the money is back")
	require.EqualValues(t, -300, systemBalance(t, a, "GOLD", ledger.RailClearing), "400 sold, 100 returned with the refund")
}
