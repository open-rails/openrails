//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
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
	t.Run("OR-J48-S08/admission-holds-the-unit-balance-and-an-overrun-is-forgiven-usage", unitOverrun)
	t.Run("OR-J48-S09/a-packs-reversals-take-what-they-took-as-they-settled", unitReversalsAsSettled)
	t.Run("OR-J48-S10/a-period-whose-price-falls-takes-back-forgiveness-first", unitPriceFalls)
	t.Run("OR-J48-S11/a-lease-sync-takes-no-usage-in-a-unit", unitLeaseUsage)
	t.Run("OR-J48-S12/the-first-balance-in-a-unit-takes-the-units-lock", unitFirstBalance)
}

// unitPaymentWorld declares GOLD and CREDITS on alpha, whose card PSP is
// Stripe; back starts the clock that long ago, so a test can advance it.
func unitPaymentWorld(t *testing.T, back time.Duration, configure ...func(*billing.MerchantDeclaration)) (*unitShop, *providerLedger) {
	t.Helper()
	a, p := newPaymentWorld(t, func(w *world) {
		w.clock.Advance(-back)
		alpha := cfgMerchant(w.cfg, "alpha")
		alpha.Currencies = []billing.MerchantCurrency{goldUnit, creditsUnit}
		for _, change := range configure {
			change(&alpha)
		}
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

	// Each lot is one deposit of all its units from units_sold: no
	// bonus from promotional funding, no cash in the unit.
	for unit, want := range map[catalog.Currency][]int64{"GOLD": {400}, "CREDITS": {2000, 5500}} {
		var deposits []int64
		for _, leg := range unitLegs(t, a, unit) {
			require.Equal(t, "deposit", leg.Type, "%+v", leg)
			require.Equal(t, string(ledger.UnitsSold), leg.Debit)
			require.Equal(t, string(ledger.CustomerUnits), leg.Credit)
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

	// The book moves: packs sold for dollars export and restore.
	archive := archiveBytes(t, a.alpha)
	_, restored := archiveTarget(t, a.world)
	_, err = restored.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
	require.NoError(t, err)
	moved, err := restored.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: s.alice})
	require.NoError(t, err)
	require.Len(t, moved.Items, 3, "every lot moves with what paid for it")
}

func unitPackInEUR(t *testing.T) {
	s, p := unitPaymentWorld(t, 0)
	a := s.a
	gold := s.pack(t, "gold-400", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"}, unitPrice{"eur", 18_000_000, "EUR"})
	var general []string
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT coalesce(array_agg(DISTINCT currency ORDER BY currency), '{}') FROM "+pgx.Identifier{a.schema, "ledger_accounts"}.Sanitize()+
		" WHERE merchant_id = $1 AND book = 'general'", a.alpha.MerchantID().UUID()).Scan(&general))
	require.Subset(t, general, []string{"EUR", "USD"}, "the general ledger opens in the money a pack sells for")
	require.NotContains(t, general, "GOLD", "a merchant's unit is never money")
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
	require.Equal(t, string(ledger.UnitsSold), legs[0].Debit, "a bought allowance is sold")
	require.Equal(t, string(ledger.UnitsSold), legs[1].Debit)
	require.Equal(t, string(ledger.UnitsGranted), legs[2].Debit, "a gift of units is granted, not sold")
	require.Equal(t, int64(-200), systemBalance(t, a, "GOLD", ledger.UnitsSold))
	require.Equal(t, int64(-25), systemBalance(t, a, "GOLD", ledger.UnitsGranted))
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
	require.Equal(t, string(ledger.CustomerUnits), spends[0].Debit)
	require.Equal(t, string(ledger.UnitsRedeemed), spends[0].Credit)
	require.EqualValues(t, 50, spends[0].Amount)
	require.Equal(t, order.ID.UUID(), *spends[0].Order, "the spend names its order")
	require.Equal(t, "order_line", spends[0].Source)
	require.Equal(t, order.Lines[0].ID.String(), spends[0].SourceID, "and its line")
	var named, total int
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FILTER (WHERE order_id = $3), count(*) FROM "+pgx.Identifier{a.schema, "ledger_transfers"}.Sanitize()+
		" WHERE merchant_id = $1 AND book = 'general' AND order_line_id = $2", a.alpha.MerchantID().UUID(), order.Lines[0].ID.UUID(), order.ID.UUID()).Scan(&named, &total))
	require.Positive(t, total, "the line's money moves in the general ledger, in dollars")
	require.Equal(t, total, named, "and its legs name the order too")

	status, raw = s.spend(t, sword, "sword")
	require.Equal(t, http.StatusOK, status, "a retry answers the order the balance paid: %s", raw)
	var again billing.Order
	require.NoError(t, json.Unmarshal(raw, &again))
	require.Equal(t, order.ID, again.ID)
	unitBalance(t, a.alpha, s.alice, "GOLD", 350, 0, 350)
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

// imageMeter prices an image at 4 GOLD.
func imageMeter(t *testing.T, a *authorityWorld) billing.ProductID {
	t.Helper()
	images, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "images", DisplayName: "Images"})
	require.NoError(t, err)
	_, err = a.alpha.SetMeter(t.Context(), "images", billing.SetMeterParams{EventType: "image", ValueProperty: "images", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: images.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "GOLD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 4}}})})
	require.NoError(t, err, "4 GOLD an image")
	return images.ID
}

// admitImages admits n images of customer's in GOLD.
func admitImages(t *testing.T, a *authorityWorld, customer billing.CustomerID, id string, n int64) billing.AdmissionVerdict {
	t.Helper()
	return admitOne(t, a.alpha, billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "GOLD",
		ExpiresAt: ptr(a.clock.Now().Add(time.Hour)), Usage: &billing.UsageEstimate{EventType: "image", Dimensions: map[string]int64{"images": n}}})
}

func unitUsage(t *testing.T) {
	s, _ := unitPaymentWorld(t, 0)
	a := s.a
	images := imageMeter(t, a)
	tokens, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "tokens", DisplayName: "Tokens"})
	require.NoError(t, err)
	_, err = a.alpha.SetMeter(t.Context(), "tokens", billing.SetMeterParams{EventType: "completion", ValueProperty: "tokens", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: tokens.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "CREDITS", PerUnit: &catalog.PerUnitPrice{UnitAmount: 100, DivideBy: 1000}}})})
	require.NoError(t, err, "1 credit per 1k tokens")
	_, err = a.alpha.SetMeter(t.Context(), "gems", billing.SetMeterParams{EventType: "gem", ValueProperty: "n", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: images, Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "GEMS"}})})
	requireCode(t, err, billing.CodeCurrencyUnsupported)

	// A rate card naming no currency is money: it prices no unit.
	pings, err := catalog.ParseApplicationYAML([]byte("schema_version: 1\nmeters:\n  pings: {event_type: ping, value_property: n, aggregation: sum}\nproducts:\n  pings: {display_name: Pings, rate_cards: [{meter: pings, price: {model: per_unit, per_unit: {unit_amount: \"1\"}}}]}\n"))
	require.NoError(t, err)
	_, err = a.alpha.ApplyCatalog(t.Context(), pings, billing.ApplyCatalogParams{})
	require.NoError(t, err)

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
	unpriced, err := a.alpha.Admit(t.Context(), []billing.AdmitParams{{RequestID: "pings", CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
		Currency: "GOLD", ExpiresAt: ptr(a.clock.Now().Add(time.Hour)), Usage: &billing.UsageEstimate{EventType: "ping", Dimensions: map[string]int64{"n": 3}}}})
	require.NoError(t, err)
	require.NotNil(t, unpriced[0].Error)
	require.Equal(t, billing.CodeUsageNotPriced, unpriced[0].Error.Code)
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

	// A top-up posts the usage first: the lot expiring soonest pays. Usage
	// at a movement's instant is not before its cut: a second passes.
	a.clock.Advance(time.Second)
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
	verdict = admitImages(t, a, thin, "over", 2)
	require.True(t, verdict.Allowed())
	captureUsage(t, a.alpha, "over", "image", map[string]int64{"images": 4}, nil)
	a.clock.Advance(time.Second)
	_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: thin, Currency: "GOLD", Amount: 5, Source: "critical", SourceID: "after"}})
	require.NoError(t, err)
	unitBalance(t, a.alpha, thin, "GOLD", 5, 0, 5)
	var arrears int
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "ledger_accounts"}.Sanitize()+
		" WHERE merchant_id = $1 AND account_type = $2 AND currency IN ('GOLD', 'CREDITS')", a.alpha.MerchantID().UUID(), string(ledger.CustomerOwed)).Scan(&arrears))
	require.Zero(t, arrears, "no arrears account exists in a unit")
	requireDenied(t, admitImages(t, a, thin, "beyond", 2), "insufficient_balance")
}

func unitOverrun(t *testing.T) {
	// A delegated invoker's failed-usage limit in dollars binds no unit.
	s, _ := unitPaymentWorld(t, 0, func(alpha *billing.MerchantDeclaration) {
		alpha.DelegatedInvokerWastedSpendLimits = []billing.BudgetWindow{{Key: "burst", WindowSeconds: 900, Limit: 5_000_000, Currency: "USD"}}
	})
	a := s.a
	imageMeter(t, a)
	_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.alice, Currency: "GOLD", Amount: 20, Source: "critical", SourceID: "twenty"}})
	require.NoError(t, err)

	// Admission holds the unit balance: once it is held, a request beyond
	// what is left, or one that holds nothing, is refused.
	require.True(t, admitImages(t, a, s.alice, "first", 3).Allowed())
	unitBalance(t, a.alpha, s.alice, "GOLD", 8, 12, 20)
	requireDenied(t, admitImages(t, a, s.alice, "beyond", 3), "insufficient_balance")
	delegated := admitOne(t, a.alpha, billing.AdmitParams{RequestID: "agent", CustomerID: s.alice, Invoker: "agent-1", InvokerType: billing.InvokerTypeDelegated, Currency: "GOLD",
		ExpiresAt: ptr(a.clock.Now().Add(time.Hour)), Usage: &billing.UsageEstimate{EventType: "image", Dimensions: map[string]int64{"images": 1}}})
	require.True(t, delegated.Allowed(), "no rate turns a dollar limit into GOLD: %+v", delegated)
	_, err = a.alpha.ReleaseAdmissions(t.Context(), []string{"agent"})
	require.NoError(t, err)
	phone := billing.AdmitParams{RequestID: "second", CustomerID: s.alice, Invoker: "alice-phone", InvokerType: billing.InvokerTypeCustomer, Currency: "GOLD",
		ExpiresAt: ptr(a.clock.Now().Add(time.Hour)), Usage: &billing.UsageEstimate{EventType: "image", Dimensions: map[string]int64{"images": 2}}}
	require.True(t, admitOne(t, a.alpha, phone).Allowed())
	unitBalance(t, a.alpha, s.alice, "GOLD", 0, 20, 20)
	requireDenied(t, admitOne(t, a.alpha, billing.AdmitParams{RequestID: "costless", CustomerID: s.alice, Invoker: s.alice.String(), InvokerType: billing.InvokerTypeCustomer,
		Currency: "GOLD", ExpiresAt: ptr(a.clock.Now().Add(time.Hour))}), "insufficient_balance")

	// The first request overruns its hold: 5 images cost 20 and the second
	// 8, against 20 in the lots. Its posting, before the next deposit, takes
	// the 20 and forgives 8: nothing is owed in a unit.
	receipt := captureUsage(t, a.alpha, "first", "image", map[string]int64{"images": 5}, nil)
	require.EqualValues(t, 20, receipt.Amount, "a capture prices its usage in full")
	receipt = captureUsage(t, a.alpha, "second", "image", map[string]int64{"images": 2}, nil)
	require.EqualValues(t, 8, receipt.Amount)
	a.clock.Advance(time.Second)
	_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.alice, Currency: "GOLD", Amount: 1, Source: "critical", SourceID: "post"}})
	require.NoError(t, err)
	unitBalance(t, a.alpha, s.alice, "GOLD", 1, 0, 1)
	var posted, forgiven int64
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT sum(amount), sum(forgiven) FROM "+pgx.Identifier{a.schema, "usage_postings"}.Sanitize()+
		" WHERE merchant_id = $1 AND customer_id = $2", a.alpha.MerchantID().UUID(), s.alice.UUID()).Scan(&posted, &forgiven))
	require.Equal(t, [2]int64{28, 8}, [2]int64{posted, forgiven}, "the posting records what it forgave")
	require.ElementsMatch(t, []forgivenShare{{Invoker: s.alice.String(), EventType: "image", MeterKey: "images", Amount: 6}, {Invoker: "alice-phone", EventType: "image", MeterKey: "images", Amount: 2}},
		forgivenShares(t, a, s.alice)[0], "and how it shared it, as it posted")
	var owed int
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "ledger_transfers"}.Sanitize()+
		" WHERE merchant_id = $1 AND customer_id = $2 AND transfer_type = 'owed_accrual'", a.alpha.MerchantID().UUID(), s.alice.UUID()).Scan(&owed))
	require.Zero(t, owed, "nothing is owed in a unit")

	require.Equal(t, map[string]int64{"charged": 20, "forgiven": 8}, usageByCharge(t, a, s.alice, a.clock.Now()), "the overrun shows as forgiven usage")
	// Each invoker's charge gives up its share of the 8, 20:8 to the largest
	// remainder: what each charged and had forgiven adds up to its usage.
	require.Equal(t, map[string]int64{s.alice.String() + "/charged": 14, s.alice.String() + "/forgiven": 6, "alice-phone/charged": 6, "alice-phone/forgiven": 2},
		usageCharged(t, a, s.alice, a.clock.Now(), "invoker"))

	status, raw := a.request(t, http.MethodGet, "/v1/me/usage?currency=GOLD", a.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var usage billing.Usage
	require.NoError(t, json.Unmarshal(raw, &usage))
	require.Len(t, usage.Rows, 1)
	require.Equal(t, "image", usage.Rows[0].Key)
	require.EqualValues(t, 28, usage.Rows[0].Amount, "the customer's usage in the unit is what it cost")
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
	require.Equal(t, string(ledger.UnitsExpired), expired[0].Credit)
	require.EqualValues(t, 350, expired[0].Amount, "what was left expires in the unit, once")

	// Usage captured while a lot lived posts before it lapses: the lot pays
	// it, and only what is left expires.
	imageMeter(t, a)
	lapsing := billing.CustomerID(uuid.New())
	_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: lapsing, Currency: "GOLD", Amount: 10, Source: "critical", SourceID: "lapsing",
		ExpiresAt: ptr(a.clock.Now().Add(time.Hour))}})
	require.NoError(t, err)
	require.True(t, admitImages(t, a, lapsing, "before-lapse", 2).Allowed())
	captured := a.clock.Now()
	captureUsage(t, a.alpha, "before-lapse", "image", map[string]int64{"images": 2}, nil)
	a.clock.Advance(26 * time.Hour)
	runRegistered(t, fleet, "openrails.billing_pass", 1)
	lot = lots(t, a.alpha, lapsing, "GOLD")[0]
	require.Equal(t, billing.CreditGrantExpired, lot.State)
	require.EqualValues(t, 8, lot.SpentAmount, "the lot pays the usage of its life")
	require.EqualValues(t, 2, lot.ExpiredAmount)
	require.Equal(t, map[string]int64{"charged": 8, "forgiven": 0}, usageByCharge(t, a, lapsing, captured))
}

// usageByCharge is customer's usage_charged in GOLD on day's UTC day, by
// charge.
func usageByCharge(t *testing.T, a *authorityWorld, customer billing.CustomerID, day time.Time) map[string]int64 {
	t.Helper()
	return usageCharged(t, a, customer, day)
}

// usageCharged is customer's usage_charged in GOLD on day's UTC day, by the
// dimensions by and then charge, keyed by their values joined with "/".
func usageCharged(t *testing.T, a *authorityWorld, customer billing.CustomerID, day time.Time, by ...string) map[string]int64 {
	t.Helper()
	on := day.UTC().Format(time.DateOnly)
	dims := append(slices.Clone(by), "charge")
	result, err := a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"usage_charged"}, By: dims,
		Filters: map[string][]string{"customer": {customer.String()}, "currency": {"GOLD"}}, Range: &billing.MetricsRange{From: on, To: on}})
	require.NoError(t, err)
	columns := map[string]int{}
	for i, c := range result.Columns {
		columns[c.Name] = i
	}
	amount, ok := columns["usage_charged"]
	require.True(t, ok, "%+v", result.Columns)
	out := map[string]int64{}
	for _, row := range result.Rows {
		var n int64
		_, err := fmt.Sscan(fmt.Sprint(row[amount]), &n)
		require.NoError(t, err)
		key := make([]string, len(dims))
		for i, d := range dims {
			c, ok := columns[d]
			require.True(t, ok, "%+v", result.Columns)
			key[i] = fmt.Sprint(row[c])
		}
		out[strings.Join(key, "/")] += n
	}
	return out
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
			require.Equal(t, string(ledger.UnitsRefunded), leg.Credit, "a reversal's units go to units_refunded")
			require.NotEqual(t, "credit_refund_cash", leg.Type, "a unit has no cash leg")
		}
	}
	require.Equal(t, map[string]int64{"credit": 300, "consumed_credit": 100}, sources)
	require.Equal(t, []int64{5_000_000, 15_000_000}, reversalCash(t, a, lot.ID.UUID()), "each reversal's cash is the general ledger's, in dollars")

	dispute["status"] = "won"
	notice("evt_gold_dispute_won", "charge.dispute.closed", dispute)
	unitBalance(t, a.alpha, s.alice, "GOLD", 200, 0, 200)
	lot = lots(t, a.alpha, s.alice, "GOLD")[0]
	require.EqualValues(t, 200, lot.RemainingAmount, "the won dispute returns what the chargeback took, not the refund's units")
	require.EqualValues(t, 100, systemBalance(t, a, "GOLD", ledger.UnitsRefunded), "the refund's 100 stay refunded; the dispute's 300 came back")
	require.EqualValues(t, -400, systemBalance(t, a, "GOLD", ledger.UnitsSold))
	require.Equal(t, []int64{5_000_000, 15_000_000, -15_000_000}, reversalCash(t, a, lot.ID.UUID()))
}

func unitReversalsAsSettled(t *testing.T) {
	s, p := unitPaymentWorld(t, 0)
	a := s.a
	gold := s.pack(t, "gold-400", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"})
	toolkit, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "toolkit", DisplayName: "Toolkit", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"toolkit:owned"}})
	require.NoError(t, err)
	toolkitPrice, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: toolkit.ID, Key: "toolkit", UnitAmount: 30_000_000, Currency: "USD"})
	require.NoError(t, err)
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: gold["usd"]}, {PriceID: toolkitPrice.ID}}, ExpectedTotal: ptr(int64(50_000_000)), Reusable: ptr(true),
		Payment: &billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "pack-and-toolkit"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	payment := *order.Payment.PaymentID
	unitBalance(t, a.alpha, s.alice, "GOLD", 400, 0, 400)

	// R1, $15 back, is sent and its answer lost: pending, it holds the 300
	// GOLD its share of the pack's $20 would take.
	p.mu.Lock()
	p.lostRefundResponse = true
	p.mu.Unlock()
	_, _ = a.alpha.RefundPayment(t.Context(), "r1", payment, billing.RefundPaymentParams{Amount: 15_000_000, Reason: "Part back"})
	unitBalance(t, a.alpha, s.alice, "GOLD", 100, 300, 400)

	// R2, a $15 chargeback, settles first: the pack's line returns $15 and
	// takes 300, leaving R1 the line's last $5, 100 GOLD.
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
	dispute := stripemock.Object{"id": "dp_order", "object": "dispute", "amount": 1500, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
	notice("evt_order_dispute", "charge.dispute.created", dispute)
	unitBalance(t, a.alpha, s.alice, "GOLD", 0, 100, 100)

	// R1 settles after it: it takes what is left of its share, not what it
	// would have taken first.
	p.mu.Lock()
	p.lostRefundResponse = false
	p.mu.Unlock()
	a.clock.Advance(20 * time.Minute)
	fleet := startFleet(t, a.world)
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
	require.NoError(t, err)
	for _, job := range jobs.Jobs {
		if job.State != rivertype.JobStateCompleted && job.State != rivertype.JobStateRunning {
			_, err = fleet.JobRetry(t.Context(), job.ID)
			require.NoError(t, err)
		}
	}
	require.Eventually(t, func() bool {
		read, err := a.alpha.GetPayment(t.Context(), payment)
		return err == nil && read.AmountRefunded == 15_000_000
	}, 20*time.Second, 20*time.Millisecond)
	unitBalance(t, a.alpha, s.alice, "GOLD", 0, 0, 0)
	lot := lots(t, a.alpha, s.alice, "GOLD")[0]
	sources := map[string]int64{}
	for _, leg := range unitLegs(t, a, "GOLD") {
		if leg.Lot != nil && *leg.Lot == lot.ID.UUID() && leg.Type != "deposit" {
			sources[leg.Source] += leg.Amount
		}
	}
	require.Equal(t, map[string]int64{"credit": 400}, sources, "the two reversals take the 400 once, none of it consumed")
	require.Equal(t, []int64{15_000_000, 5_000_000}, reversalCash(t, a, lot.ID.UUID()), "each reversal's share as it settled, recorded once in the general ledger")

	// Winning R2's dispute restores what R2 took: the line has $15 open,
	// 300 GOLD.
	dispute["status"] = "won"
	notice("evt_order_dispute_won", "charge.dispute.closed", dispute)
	unitBalance(t, a.alpha, s.alice, "GOLD", 300, 0, 300)
	require.Equal(t, []int64{15_000_000, 5_000_000, -15_000_000}, reversalCash(t, a, lot.ID.UUID()))
	require.EqualValues(t, 100, systemBalance(t, a, "GOLD", ledger.UnitsRefunded), "400 refunded, 300 restored")
}

// reversalCash is the cash each reversal of lot's payment returned, in the
// general ledger, in the order they applied: a recovery's is negative.
func reversalCash(t *testing.T, a *authorityWorld, lot uuid.UUID) []int64 {
	t.Helper()
	transfers, accounts := pgx.Identifier{a.schema, "ledger_transfers"}.Sanitize(), pgx.Identifier{a.schema, "ledger_accounts"}.Sanitize()
	rows, err := a.pool.Query(t.Context(), "SELECT sum(CASE WHEN c.account_type IN ('cash_clearing', 'external_receipts') THEN t.amount ELSE -t.amount END)::bigint FROM "+transfers+" t"+
		" JOIN "+accounts+" d ON d.merchant_id = t.merchant_id AND d.id = t.debit_account_id JOIN "+accounts+" c ON c.merchant_id = t.merchant_id AND c.id = t.credit_account_id"+
		" WHERE t.merchant_id = $1 AND t.credit_grant_id = $2 AND t.book = 'general' AND t.operation = 'purchased_credit_refund'"+
		" AND (c.account_type IN ('cash_clearing', 'external_receipts') OR d.account_type IN ('cash_clearing', 'external_receipts'))"+
		" GROUP BY t.source_id ORDER BY min(t.created_at), min(t.id::text)", a.alpha.MerchantID().UUID(), lot)
	require.NoError(t, err)
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var cash int64
		require.NoError(t, rows.Scan(&cash))
		out = append(out, cash)
	}
	require.NoError(t, rows.Err())
	return out
}

func unitPriceFalls(t *testing.T) {
	s, _ := unitPaymentWorld(t, 0)
	a := s.a
	earlyInMonth(a.world)
	images, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "images", DisplayName: "Images"})
	require.NoError(t, err)
	// 4 GOLD an image up to 5, 1 each once there are more.
	price := tiers(catalog.TierModeVolume, 4, 1, 5)
	price.Currency = "GOLD"
	_, err = a.alpha.SetMeter(t.Context(), "images", billing.SetMeterParams{EventType: "image", ValueProperty: "images", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: images.ID, Price: price})})
	require.NoError(t, err)
	grant := func(id string, amount int64) {
		t.Helper()
		_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.alice, Currency: "GOLD", Amount: amount, Source: "critical", SourceID: id}})
		require.NoError(t, err)
	}
	grant("ten", 10)
	require.True(t, admitImages(t, a, s.alice, "five", 2).Allowed())
	captureUsage(t, a.alpha, "five", "image", map[string]int64{"images": 5}, nil)
	// Five images cost 20: the posting takes the 10 and forgives 10.
	a.clock.Advance(time.Second)
	grant("post", 1)
	unitBalance(t, a.alpha, s.alice, "GOLD", 1, 0, 1)
	// A sixth brings the period's price to 6: it posted 20.
	require.True(t, admitImages(t, a, s.alice, "sixth", 1).Allowed())
	captureUsage(t, a.alpha, "sixth", "image", map[string]int64{"images": 1}, nil)
	fleet := startFleet(t, a.world)
	nextMonth(a.world)
	closeMonths(t, fleet)
	type posting struct{ amount, forgiven int64 }
	postings := func() []posting {
		rows, err := a.pool.Query(t.Context(), "SELECT amount, forgiven FROM "+pgx.Identifier{a.schema, "usage_postings"}.Sanitize()+
			" WHERE merchant_id = $1 AND customer_id = $2 ORDER BY created_at, id", a.alpha.MerchantID().UUID(), s.alice.UUID())
		require.NoError(t, err)
		defer rows.Close()
		var out []posting
		for rows.Next() {
			var r posting
			require.NoError(t, rows.Scan(&r.amount, &r.forgiven))
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}
	require.Equal(t, []posting{{20, 10}}, postings(), "the fall is carried while late usage may come")
	// Once it closes, the 14 the period posted beyond its price takes back
	// the 10 forgiven first; only the 4 the lots paid beyond it comes back.
	a.clock.Advance(70 * 24 * time.Hour)
	closeMonths(t, fleet)
	require.Equal(t, []posting{{20, 10}, {-14, -10}}, postings())
	require.Equal(t, [][]forgivenShare{{{Invoker: s.alice.String(), EventType: "image", MeterKey: "images", Amount: 10}}, {{Invoker: s.alice.String(), EventType: "image", MeterKey: "images", Amount: -10}}},
		forgivenShares(t, a, s.alice), "the reversal takes back from the shares the posting stored")
	unitBalance(t, a.alpha, s.alice, "GOLD", 5, 0, 5)
}

func unitLeaseUsage(t *testing.T) {
	s, _ := unitPaymentWorld(t, 0)
	a := s.a
	imageMeter(t, a)
	_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.alice, Currency: "GOLD", Amount: 20, Source: "critical", SourceID: "twenty"}})
	require.NoError(t, err)
	item := billing.LeaseUsage{CustomerID: s.alice, Invoker: s.alice.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "GOLD", EventType: "image",
		Dimensions: map[string]int64{"images": 1}, Source: "critical", SourceID: "gate"}
	out := syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Usage: numbered(1, item)})
	require.Equal(t, billing.CodeCurrencyUnsupported, refusedItem(t, out, 1).Error.Code, "usage in a unit comes through an admission's capture")
	unitBalance(t, a.alpha, s.alice, "GOLD", 20, 0, 20)
}

func unitFirstBalance(t *testing.T) {
	s, _ := unitPaymentWorld(t, 0)
	a := s.a
	hold := func() pgx.Tx {
		t.Helper()
		tx, err := a.pool.Begin(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		_, err = tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", ledger.UnitsLock(a.alpha.MerchantID().UUID()))
		require.NoError(t, err)
		return tx
	}
	give := func(id string) chan error {
		done := make(chan error, 1)
		go func() {
			_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.alice, Currency: "GOLD", Amount: 10, Source: "critical", SourceID: id}})
			done <- err
		}()
		return done
	}
	// Alice is a customer already.
	_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.alice, Currency: "USD", Amount: 1_000_000, Source: "critical", SourceID: "dollar"}})
	require.NoError(t, err)
	// A settings change removing GOLD holds the units lock: the gift that
	// opens the first GOLD balance waits for it, before it locks alice, so
	// her other money waits for nothing.
	tx := hold()
	first := give("first")
	select {
	case err := <-first:
		t.Fatalf("the first GOLD balance did not wait for the units lock: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	probe, err := a.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = probe.Exec(t.Context(), "SELECT id FROM "+pgx.Identifier{a.schema, "customers"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2 FOR NO KEY UPDATE NOWAIT",
		a.alpha.MerchantID().UUID(), s.alice.UUID())
	require.NoError(t, err, "the waiting gift holds no customer lock")
	require.NoError(t, probe.Rollback(t.Context()))
	require.NoError(t, tx.Rollback(t.Context()))
	select {
	case err := <-first:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the gift did not finish once the lock was free")
	}
	var scale int
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT scale FROM "+pgx.Identifier{a.schema, "ledger_units"}.Sanitize()+" WHERE merchant_id = $1 AND unit = 'GOLD'",
		a.alpha.MerchantID().UUID()).Scan(&scale))
	require.Zero(t, scale, "the book records GOLD at the scale it was declared")

	// Once GOLD is counted, a gift takes no lock.
	hold()
	select {
	case err := <-give("second"):
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("a gift in a counted unit waited for the units lock")
	}
	unitBalance(t, a.alpha, s.alice, "GOLD", 20, 0, 20)
}

type forgivenShare struct {
	Invoker   string `json:"invoker"`
	EventType string `json:"event_type"`
	MeterKey  string `json:"meter_key"`
	Cell      string `json:"cell"`
	Amount    int64  `json:"amount"`
}

// forgivenShares are how each of customer's postings that forgave shared it,
// in posting order.
func forgivenShares(t *testing.T, a *authorityWorld, customer billing.CustomerID) [][]forgivenShare {
	t.Helper()
	rows, err := a.pool.Query(t.Context(), "SELECT forgiven_lines FROM "+pgx.Identifier{a.schema, "usage_postings"}.Sanitize()+
		" WHERE merchant_id = $1 AND customer_id = $2 AND forgiven <> 0 ORDER BY created_at, id", a.alpha.MerchantID().UUID(), customer.UUID())
	require.NoError(t, err)
	defer rows.Close()
	var out [][]forgivenShare
	for rows.Next() {
		var raw []byte
		require.NoError(t, rows.Scan(&raw))
		var shares []forgivenShare
		require.NoError(t, json.Unmarshal(raw, &shares))
		out = append(out, shares)
	}
	require.NoError(t, rows.Err())
	return out
}
