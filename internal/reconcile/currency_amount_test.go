package reconcile

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// NMI's schedule names no currency: its amount is read in the billed price's
// currency. ¥500 arrives as "500.00" and matches a ¥500 price; a fractional yen
// is drift, never a rounding.
func TestScheduleAmountIsReadInThePriceCurrency(t *testing.T) {
	for _, tc := range []struct {
		name, currency, remote string
		price                  int64
		drift                  map[string]any
	}{
		{"yen", "JPY", "500.00", 5_000_000, nil},
		{"won", "KRW", "1500", 15_000_000, nil},
		{"dollars", "USD", "9.99", 9_990_000, nil},
		{"no amount", "USD", "", 9_990_000, nil},
		{"zero override", "USD", "0.00", 9_990_000, nil},
		{"yen changed", "JPY", "600.00", 5_000_000, map[string]any{"local_amount": "500 JPY", "remote_amount": "600.00 JPY"}},
		{"fractional yen", "JPY", "500.50", 5_000_000, map[string]any{"local_amount": "500 JPY", "remote_amount": "500.50 JPY"}},
		{"sub-cent", "USD", "9.999", 9_990_000, map[string]any{"local_amount": "9.99 USD", "remote_amount": "9.999 USD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			priceID := uuid.New()
			local := &LocalState{
				Subscriptions: []LocalSubscription{{ID: uuid.New(), PriceID: &priceID, Status: "active", RailSubscriptionID: "rs"}},
				Prices:        []LocalPrice{{ID: priceID, Amount: tc.price, Currency: tc.currency}},
			}
			idx := buildLocalIndex(local)
			f := compareScheduleTerms(ProviderNMI, idx, &local.Subscriptions[0], &RemoteSubscription{RailSubscriptionID: "rs", Status: SubscriptionStatusActive, Amount: tc.remote})
			if tc.drift == nil {
				require.Nil(t, f)
				return
			}
			require.NotNil(t, f)
			require.Equal(t, tc.drift, f.RemoteEvidence["drift"])
		})
	}
}

// A provider that names a currency is read at fetch; one that names none
// (CCBill DataLink) keeps its verbatim decimal until the matched record
// denominates it: "500.00" is ¥500 = 5_000_000 native in JPY, $500 in USD.
func TestRemoteAmountIsReadInTheRecordedCurrency(t *testing.T) {
	named := RemoteTransaction{Currency: "JPY"}
	named.setAmount("500.00")
	require.Equal(t, RemoteTransaction{Currency: "JPY", AmountCents: 500}, named)
	inexact := RemoteTransaction{Currency: "JPY"}
	inexact.setAmount("500.50")
	require.Zero(t, inexact.AmountCents, "refused, never rounded")
	require.False(t, inexact.positive())

	verbatim := RemoteTransaction{}
	verbatim.setAmount(" 500.00 ")
	require.Equal(t, RemoteTransaction{Amount: "500.00"}, verbatim, "no currency is never read as USD")
	require.True(t, verbatim.positive())
	for currency, want := range map[string]int64{"JPY": 5_000_000, "USD": 500_000_000} {
		native, err := verbatim.nativeIn(currency)
		require.NoError(t, err)
		require.Equal(t, want, native, currency)
	}
	_, err := verbatim.nativeIn("")
	require.Error(t, err, "a row no record denominates has no amount")
	_, err = (&RemoteTransaction{Amount: "500.50"}).minorIn("JPY")
	require.Error(t, err)
	for amount, positive := range map[string]bool{"9.99": true, "0.00": false, "-5.00": false, "abc": false} {
		require.Equal(t, positive, (&RemoteTransaction{Amount: amount}).positive(), amount)
	}

	// Undenominated, the charge is never backfilled under a guessed currency.
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	priceID := uuid.New()
	sub := LocalSubscription{ID: uuid.New(), CustomerID: uuid.New(), PriceID: &priceID, Status: "active", Rail: "ccbill", RailSubscriptionID: "rs", StartedAt: now.Add(-time.Hour)}
	snap := &RemoteSnapshot{Provider: ProviderCCBill, Capabilities: Capabilities{Transactions: true}, Transactions: []RemoteTransaction{
		{TransactionID: "rebill", SubscriptionID: "rs", Type: TransactionTypeSale, Success: true, Amount: "500.00", OccurredAt: now},
	}}
	var charge *Finding
	for _, f := range diffProvider(ProviderCCBill, snap, &LocalState{Subscriptions: []LocalSubscription{sub}}, nil, now, diffOptions{}) {
		if f.Type == FindingChargeMissingLocal {
			charge = &f
		}
	}
	require.NotNil(t, charge)
	require.Nil(t, charge.Apply)
	require.Equal(t, FindingStatusAdminRequired, charge.Status)
	require.Equal(t, "500.00 (no currency reported)", charge.RemoteEvidence["amount"])
}
