package webhooks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// #671 TEST WALL — CCBill inbound wire pinning. CCBill is an inbound-only rail:
// webhook payloads carry decimal amount strings ("19.99"). The pipeline is
// parseCCBillAmountCents (big.Rat exact ⇒ cents) then CentsToMicros for the
// stored payment row, with a ±2% amount-mismatch threshold that must trip the
// repair alert on real drift. Only parsing/conversion helpers are pinned here
// (grace/entitlement behavior is owned elsewhere).

func TestParseCCBillAmountCents_WirePin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want moneyutil.Cents // cents, literal
	}{
		{"19.99", 1999},
		{"0.01", 1},
		{"10", 1000},
		{"129.95", 12995},
		{"0.10", 10},
		{"1999.00", 199900}, // a raw-cents-looking string is still DOLLARS
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			got, err := parseCCBillAmountCents(tc.raw, "billedInitialPrice", "billedAmount", false)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseCCBillAmountCents_RejectsGarbageAndNegatives(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "abc", "-5.00", "19.99USD", "$19.99", "1,999.00"} {
		t.Run("rejects "+raw, func(t *testing.T) {
			t.Parallel()
			_, err := parseCCBillAmountCents(raw, "billedInitialPrice", "billedAmount", false)
			require.Error(t, err)
		})
	}
	// Zero is only valid when the event may legitimately bill nothing (trial).
	_, err := parseCCBillAmountCents("0.00", "billedInitialPrice", "billedAmount", false)
	require.Error(t, err)
}

// TestCCBillBilledAmountToStoredMicros_WirePin pins the full inbound chain the
// success handlers use for the stored payment row: decimal wire string ⇒ exact
// cents ⇒ micros (handleNewSaleSuccessInternal / handleUpgradeSuccess store
// CentsToMicros(billedAmountCents)).
func TestCCBillBilledAmountToStoredMicros_WirePin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw        string
		wantMicros moneyutil.Micros // literal
	}{
		{"19.99", 19_990_000},
		{"0.01", 10_000},
		{"14.95", 14_950_000},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			cents, err := parseCCBillAmountCents(tc.raw, "billedInitialPrice", "billedAmount", false)
			require.NoError(t, err)
			require.Equal(t, tc.wantMicros, moneyutil.CentsToMicros(cents))
		})
	}
}

// TestValidateCCBillBilledAmount_TwoPercentThreshold_WirePin pins the exact
// tolerance window for a $19.99 price (expected 1999 cents ⇒ tolerance 39
// cents ⇒ accepted [1960, 2038]) and that crossing it yields the amount
// BillingError — the same error that records the ledger-repair alert when a
// service is attached (#675).
func TestValidateCCBillBilledAmount_TwoPercentThreshold_WirePin(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const expectedMicros = moneyutil.Micros(19_990_000) // $19.99 catalog price

	parse := func(t *testing.T, raw string) moneyutil.Cents {
		t.Helper()
		cents, err := parseCCBillAmountCents(raw, "billedAmount", "billedAmount", false)
		require.NoError(t, err)
		return cents
	}

	// Within ±2%: exact, and both inclusive boundaries.
	for _, raw := range []string{"19.99", "19.60", "20.38"} {
		require.NoError(t, validateCCBillBilledAmount(ctx, nil, "USD", parse(t, raw), expectedMicros, nil, nil), raw)
	}

	// One cent past either boundary: the mismatch threshold fires.
	for _, raw := range []string{"19.59", "20.39"} {
		err := validateCCBillBilledAmount(ctx, nil, "USD", parse(t, raw), expectedMicros, nil, nil)
		require.Error(t, err, raw)
		var billingErr *BillingError
		require.True(t, errors.As(err, &billingErr), raw)
		require.Equal(t, ErrorTypeAmount, billingErr.Type)
		require.Equal(t, moneyutil.Cents(1999), billingErr.Context["expected_amount_cents"])
		require.Equal(t, moneyutil.Cents(39), billingErr.Context["tolerance_cents"])
	}

	// A 10,000×-style unit mixup (micros where cents belong) is FAR outside 2%.
	err := validateCCBillBilledAmount(ctx, nil, "USD", 19_990_000, expectedMicros, nil, nil)
	require.Error(t, err)

	// A sub-cent expected amount cannot be compared in whole cents: loud error,
	// never rounding.
	err = validateCCBillBilledAmount(ctx, nil, "USD", 1999, 19_995_000, nil, nil)
	require.ErrorContains(t, err, "whole cents")
}
