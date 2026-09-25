package webhooks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// CCBill posts decimal dollar strings; they must land as exact cents, then micros.
func TestCCBillAmountWireParsing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw       string
		cents     moneyutil.Cents
		micros    moneyutil.Micros
		allowZero bool
		wantErr   bool
	}{
		{raw: "19.99", cents: 1999, micros: 19_990_000},
		{raw: "0.01", cents: 1, micros: 10_000},
		{raw: "10", cents: 1000, micros: 10_000_000},
		{raw: "0.10", cents: 10, micros: 100_000},
		{raw: "1999.00", cents: 199900, micros: 1_999_000_000}, // looks like cents, is dollars
		{raw: "0.00", allowZero: true},
		{raw: "0.00", wantErr: true},
		{raw: "-5.00", wantErr: true},
		{raw: "-5.00", allowZero: true, wantErr: true},
		{raw: "", wantErr: true},
		{raw: "abc", wantErr: true},
		{raw: "19.99USD", wantErr: true},
		{raw: "$19.99", wantErr: true},
		{raw: "1,999.00", wantErr: true},
	} {
		t.Run(fmt.Sprintf("%q/zero=%v", tc.raw, tc.allowZero), func(t *testing.T) {
			got, err := parseCCBillAmountCents(tc.raw, "billedAmount", "billedAmount", tc.allowZero)
			if tc.wantErr {
				require.Error(t, err)
				require.True(t, shouldTreatCCBillErrorAsNonRetryable(err), "a malformed amount never changes on redelivery")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.cents, got)
			require.Equal(t, tc.micros, moneyutil.CentsToMicros(got))
		})
	}
}

// ±2% of the catalog price, inclusive, integer-only; outside it is an amount BillingError.
func TestCCBillBilledAmountTolerance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const price = moneyutil.Micros(19_990_000) // tolerance = 1999*2/100 = 39 cents

	for _, billed := range []moneyutil.Cents{1999, 1960, 2038} {
		require.NoError(t, validateCCBillBilledAmount(ctx, nil, "USD", billed, price, nil, nil), billed)
	}
	for _, billed := range []moneyutil.Cents{1959, 2039, 19_990_000} {
		err := validateCCBillBilledAmount(ctx, nil, "USD", billed, price, map[string]interface{}{"subscription_id": "sub_1"}, nil)
		var be *BillingError
		require.True(t, errors.As(err, &be), billed)
		require.Equal(t, ErrorTypeAmount, be.Type)
		require.Equal(t, moneyutil.Cents(1999), be.Context["expected_amount_cents"])
		require.Equal(t, billed, be.Context["billed_amount_cents"])
		require.Equal(t, moneyutil.Cents(39), be.Context["tolerance_cents"])
		require.Equal(t, "sub_1", be.Context["subscription_id"])
		require.True(t, shouldTreatCCBillErrorAsNonRetryable(err))
	}

	// A sub-cent expected price cannot be compared in whole cents: refuse, never round.
	require.ErrorContains(t, validateCCBillBilledAmount(ctx, nil, "USD", 1999, 19_995_000, nil, nil), "whole cents")
	// JPY has no minor unit: 1000 yen = 1_000 * 10_000 internal units.
	require.NoError(t, validateCCBillBilledAmount(ctx, nil, "JPY", 1000, 10_000_000, nil, nil))
}

// CUR-6: the ingestion boundary returns the canonical upper-case alpha code.
func TestCCBillCurrencyIngestion(t *testing.T) {
	t.Parallel()
	for raw, want := range map[Stringish]string{"840": "USD", " 978 ": "EUR", "036": "AUD", "392": "JPY", "usd": "USD", "xyz": "XYZ"} {
		got, err := requireCCBillCurrency(raw, "billedCurrencyCode")
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	_, err := requireCCBillCurrency("  ", "billedCurrencyCode")
	var be *BillingError
	require.True(t, errors.As(err, &be))
	require.Equal(t, ErrorTypeValidation, be.Type)

	require.NoError(t, validateCCBillCurrencyMatches("USD", "usd", nil))
	require.NoError(t, validateCCBillCurrencyMatches("eur", "", nil))
	err = validateCCBillCurrencyMatches("eur", "usd", map[string]interface{}{"subscription_id": "sub_1"})
	require.True(t, errors.As(err, &be))
	require.Equal(t, ErrorTypeValidation, be.Type)
	require.Equal(t, "EUR", be.Context["billed_currency"])
	require.Equal(t, "USD", be.Context["expected_currency"])
	require.Equal(t, "sub_1", be.Context["subscription_id"])
}

func TestCCBillPriceSelection(t *testing.T) {
	t.Parallel()
	intro, trialHours := int64(19_950_000), 720
	require.Equal(t, moneyutil.Micros(19_950_000), ccbillInitialChargeAmount(&models.Price{Amount: 14_950_000, AutoRenew: true, TrialUnitAmount: &intro, TrialDurationHours: &trialHours}))
	require.Equal(t, moneyutil.Micros(14_950_000), ccbillInitialChargeAmount(&models.Price{Amount: 14_950_000, AutoRenew: true}))
	require.Zero(t, ccbillInitialChargeAmount(nil))

	require.Equal(t, "0000007498", ccbillPriceLookupID(" 0000007498 ", "flex-123"))
	require.Equal(t, "flex-123", ccbillPriceLookupID(" ", " flex-123 "))
}

func TestCCBillDates(t *testing.T) {
	t.Parallel()
	// Date-only fields mean the END of that UTC day, so no access gap opens.
	got, err := parseCCBillDateUsingTimestamp("2026-03-15")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 3, 15, 23, 59, 59, 0, time.UTC), *got)
	got, err = parseCCBillDateUsingTimestamp(" ")
	require.NoError(t, err)
	require.Nil(t, got)
	_, err = parseCCBillDateUsingTimestamp("2026-31-99")
	require.Error(t, err)

	paidEnd := time.Date(2026, 5, 1, 23, 59, 59, 0, time.UTC)
	within := paidEnd.Add(ccbillGraceCap - time.Second)
	require.Equal(t, within, *capCCBillRetryAt(&within, &paidEnd))
	far := paidEnd.Add(30 * 24 * time.Hour)
	require.Equal(t, paidEnd.Add(ccbillGraceCap), *capCCBillRetryAt(&far, &paidEnd))
	require.Equal(t, far, *capCCBillRetryAt(&far, nil))
	require.Nil(t, capCCBillRetryAt(nil, &paidEnd))
}

// SEC-33 fail-closed branches; the bounded happy path is greenfield TestSecurityCCBillPeriodEndsAreBounded.
func TestBoundCCBillPeriodEndFailsClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	cycle := 720
	paidEnd := now.Add(10 * 24 * time.Hour)
	sub := &models.Subscription{CurrentPeriodEndsAt: &paidEnd, Price: &models.Price{AutoRenew: true, AccessDurationHours: &cycle}}

	got, err := boundCCBillPeriodEnd(nil, sub, now)
	require.NoError(t, err)
	require.Nil(t, got)

	near := paidEnd.Add(24 * time.Hour)
	got, err = boundCCBillPeriodEnd(&near, sub, now)
	require.NoError(t, err)
	require.Equal(t, near, *got)

	far := now.AddDate(50, 0, 0)
	got, err = boundCCBillPeriodEnd(&far, sub, now)
	require.NoError(t, err)
	require.Equal(t, paidEnd.Add(720*time.Hour+ccbillGraceCap), *got)

	lapsed := now.Add(-48 * time.Hour)
	got, err = boundCCBillPeriodEnd(&far, &models.Subscription{CurrentPeriodEndsAt: &lapsed, Price: sub.Price}, now)
	require.NoError(t, err)
	require.Equal(t, now.Add(720*time.Hour+ccbillGraceCap), *got, "a lapsed period anchors at now")

	for name, s := range map[string]*models.Subscription{
		"no subscription": nil,
		"no price":        {},
		"no cycle":        {Price: &models.Price{AutoRenew: false, AccessDurationHours: &cycle}},
	} {
		_, err := boundCCBillPeriodEnd(&far, s, now)
		require.Error(t, err, name)
		require.True(t, IsWebhookErrorNonRetryable(err), name)
	}
}

func TestCCBillDeclinedPeriod(t *testing.T) {
	t.Parallel()
	paid := time.Date(2026, 5, 1, 7, 30, 0, 0, time.UTC)
	sameDay, dayBefore, later := time.Date(2026, 5, 1, 23, 59, 59, 0, time.UTC), time.Date(2026, 4, 30, 23, 59, 59, 0, time.UTC), time.Date(2026, 5, 3, 23, 59, 59, 0, time.UTC)
	sub := &models.Subscription{CurrentPeriodEndsAt: &paid}
	require.Equal(t, paid, ccbillDeclinedPeriod(sub, &sameDay), "the unpaid period")
	require.Equal(t, paid, ccbillDeclinedPeriod(sub, &later), "a retry of the unpaid period")
	require.Equal(t, paid, ccbillDeclinedPeriod(sub, nil))
	require.Equal(t, dayBefore, ccbillDeclinedPeriod(sub, &dayBefore), "a period already paid")
	require.True(t, ccbillDeclinedPeriod(&models.Subscription{}, &later).IsZero())
}

func TestCCBillErrorRetryClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err      error
		terminal bool
	}{
		{nil, false},
		{newBillingError(ErrorTypeValidation, "x", nil, nil), true},
		{newBillingError(ErrorTypeAmount, "x", nil, nil), true},
		{newBillingError(ErrorTypeDuplicate, "x", nil, nil), true},
		{newBillingError(ErrorTypeStatusChange, "x", nil, nil), true},
		{newBillingError(ErrorTypeBusinessLogic, "x", nil, nil), false},
		{newBillingError(ErrorTypeDatabase, "x", nil, nil), false},
		{newBillingError(ErrorTypeNotFound, "x", nil, nil), false},
		{fmt.Errorf("get subscription: %w", sql.ErrNoRows), false}, // out-of-order delivery: retry
		{errors.New("payment form id mismatch: got a, want b"), true},
		{errors.New("failed to parse nextRenewalDate"), true},
		{errors.New("connection reset"), false},
	} {
		require.Equal(t, tc.terminal, shouldTreatCCBillErrorAsNonRetryable(tc.err), "%v", tc.err)
	}

	err := (&CCBillWebhookService{}).handleRenewalSuccessInternal(context.Background(), &CCBillRenewalSuccessEvent{})
	require.True(t, shouldTreatCCBillErrorAsNonRetryable(err), "renewal without transactionId is terminal")
}

// Dedupe keys: transactionId wins; otherwise a hash of the canonical (key-sorted) body.
func TestCCBillStableDedupeKey(t *testing.T) {
	t.Parallel()
	key := func(eventType string, body string) string {
		return (&CCBillWebhookService{Data: CCBillWebhookEvent{EventType: eventType, EventBody: []byte(body)}}).stableDedupeEventKey()
	}
	require.Equal(t, "tx:txn_123", key(EventTypeRenewalSuccess, `{"transactionId":" txn_123 ","subscriptionId":"sub_1"}`))

	a := key(EventTypeCancellation, `{"subscriptionId":"sub_1","timestamp":"2026-02-17 12:00:00","reason":"user"}`)
	b := key(EventTypeCancellation, `{"reason":"user","timestamp":"2026-02-17 12:00:00","subscriptionId":"sub_1"}`)
	require.Equal(t, a, b)
	require.Contains(t, a, "ccbill:event:")
	require.NotEqual(t, a, key(EventTypeCancellation, `{"subscriptionId":"sub_2","timestamp":"2026-02-17 12:00:00","reason":"user"}`))

	require.Contains(t, key(EventTypeCancellation, `not json`), "ccbill:raw:")
	require.Contains(t, key(EventTypeCancellation, ``), "ccbill:empty:")
}

// CCBill signs nothing: the armed account identity is the per-merchant auth, and it fails closed.
func TestCCBillWebhookAuthFailsClosed(t *testing.T) {
	t.Parallel()
	armed := ccbill.NewRESTClient(&config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0000"})
	for name, tc := range map[string]struct {
		client *ccbill.RESTClient
		body   string
	}{
		"wrong account":    {armed, `{"subscriptionId":"s","transactionId":"t","clientAccnum":"111111","clientSubacc":"0000"}`},
		"wrong subaccount": {armed, `{"subscriptionId":"s","transactionId":"t","clientAccnum":945280,"clientSubacc":"0001"}`},
		"no armed account": {nil, `{"subscriptionId":"s","transactionId":"t","clientAccnum":"945280","clientSubacc":"0000"}`},
		"unparseable":      {armed, `{`},
	} {
		svc := &CCBillWebhookService{Data: CCBillWebhookEvent{EventType: EventTypeRenewalSuccess, EventBody: []byte(tc.body)}, CCBillClient: tc.client}
		err := svc.HandleCCBillWebhook(context.Background())
		require.Error(t, err, name)
		require.True(t, IsWebhookErrorNonRetryable(err), name)
	}
}
