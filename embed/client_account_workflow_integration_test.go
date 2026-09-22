//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/sirupsen/logrus"
)

func TestClientAccountAndUsageWorkflow(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	for _, d := range clientWorkflowDeployments(t, h) {
		t.Run(d.name, func(t *testing.T) {
			client := d.client
			customer, err := client.EnsureCustomer(ctx, uuid.NewString())
			require.NoError(t, err)
			payer := customer.ID
			depositRequest := openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "client-contract", Currency: "USD", Amount: 1_000_000, Source: "conformance", SourceID: uuid.NewString(), Description: "seed"}
			deposit, err := client.DepositCredits(ctx, depositRequest)
			require.NoError(t, err)
			require.NotEmpty(t, deposit.ID)
			require.Equal(t, payer, deposit.CustomerID)
			require.False(t, deposit.CreatedAt.IsZero())
			require.EqualValues(t, 1_000_000, deposit.Amount)
			require.Equal(t, "deposit", deposit.TransactionType)
			require.Equal(t, "posted", deposit.Status)
			require.Equal(t, "conformance", deposit.Source)
			require.Equal(t, "client-contract", deposit.Invoker)
			require.Nil(t, deposit.BalanceAfter)
			require.Nil(t, deposit.Captured)
			require.False(t, deposit.Replayed)
			repeated, err := client.DepositCredits(ctx, depositRequest)
			require.NoError(t, err)
			require.True(t, repeated.Replayed)
			require.Equal(t, deposit.Amount, repeated.Amount)

			resource, requestID := "resource-"+uuid.NewString(), "hold-"+uuid.NewString()
			deadline := microInstant(time.Now().Add(time.Hour))
			admission := openrails.AdmitRequest{CustomerID: payer, Invoker: "client-contract", InvokerType: openrails.InvokerTypePayer, Currency: "USD", EstimatedAmount: 10_000, ExpiresAt: &deadline, Source: "conformance", RequestID: requestID}
			verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{admission})
			require.NoError(t, err)
			require.Len(t, verdicts, 1)
			allowed := verdicts[0].Result
			require.NotNil(t, allowed)
			require.True(t, allowed.Allowed)
			require.Empty(t, allowed.BlockedBy)
			require.Empty(t, allowed.DenyCode)
			require.Zero(t, allowed.RetryAfterSeconds)
			require.NotNil(t, allowed.HoldExpiresAt)
			require.True(t, allowed.HoldExpiresAt.Equal(deadline))
			replay, err := client.Admit(ctx, admission)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			require.NoError(t, client.ExtendHold(ctx, requestID, microInstant(deadline.Add(30*time.Minute))))
			require.ErrorIs(t, client.ExtendHold(ctx, requestID, microInstant(time.Now().Add(-time.Hour))), openrails.ErrInvalid)
			receipt, err := client.Capture(ctx, requestID, 8_000, &openrails.CaptureUsage{EventType: "invoke", Resource: resource})
			require.NoError(t, err)
			require.EqualValues(t, 8_000, receipt.Amount)
			admission.RequestID, admission.EstimatedAmount = uuid.NewString(), 10_000_000_000
			denied, err := client.Admit(ctx, admission)
			require.NoError(t, err, "a denial is a verdict")
			require.False(t, denied.Allowed)
			require.Equal(t, "money", denied.BlockedBy)
			require.Equal(t, "insufficient_balance", denied.DenyCode)
			require.Zero(t, denied.RetryAfterSeconds)
			require.Nil(t, denied.HoldExpiresAt)

			balance, err := client.Balance(ctx, payer)
			require.NoError(t, err)
			account, err := client.GetCreditAccount(ctx, payer, "USD")
			require.NoError(t, err)
			for _, got := range []*openrails.CreditAccount{balance, account} {
				require.Equal(t, payer, got.CustomerID)
				require.Equal(t, "USD", got.Currency)
				require.Equal(t, "prepaid", got.BillingMode)
				require.EqualValues(t, 992_000, got.BalanceAmount)
				require.EqualValues(t, 992_000, got.AvailableAmount)
				require.Zero(t, got.HeldAmount)
				require.Zero(t, got.OutstandingOwedAmount)
			}
			from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			usage, err := client.UsageRollup(ctx, payer, "USD", from, to, "resource")
			require.NoError(t, err)
			require.Equal(t, []openrails.UsageRollupRow{{Key: resource, Currency: "USD", EventCount: 1, TotalAmount: 8_000}}, usage)
			revenue, err := client.ResourceRevenueDaily(ctx, resource, "USD", from, to)
			require.NoError(t, err)
			require.Equal(t, "USD", revenue.Currency)
			require.EqualValues(t, 8_000, revenue.RevenueAmount)
			require.Len(t, revenue.Daily, 1)
			require.NotEmpty(t, revenue.Daily[0].Date)
			require.EqualValues(t, 8_000, revenue.Daily[0].Amount)
			for _, unknown := range []string{uuid.NewString(), "not-a-uuid"} {
				require.ErrorIs(t, client.Release(ctx, unknown), openrails.ErrNotFound)
			}

			batchKey := uuid.NewString()
			verdicts, err = client.AdmitBatch(ctx, []openrails.AdmitRequest{
				{CustomerID: payer, Invoker: "client-contract", InvokerType: openrails.InvokerTypePayer, Currency: "USD", EstimatedAmount: 0, RequestID: batchKey, Source: "admit"},
				{CustomerID: payer, Invoker: "client-contract", InvokerType: openrails.InvokerTypePayer, Currency: "USD", EstimatedAmount: 10_000_000_000, ExpiresAt: &deadline, RequestID: uuid.NewString()},
				{Invoker: "client-contract", InvokerType: openrails.InvokerTypePayer, Currency: "USD", EstimatedAmount: 1, ExpiresAt: &deadline, RequestID: uuid.NewString()},
				{CustomerID: payer, Invoker: "client-contract", InvokerType: openrails.InvokerTypePayer, Currency: "USD", EstimatedAmount: -1, RequestID: uuid.NewString()},
			})
			require.NoError(t, err)
			require.Len(t, verdicts, 4)
			for i, status := range []int{200, 402, 400, 400} {
				require.Equal(t, status, verdicts[i].Status)
				if i < 2 {
					require.Nil(t, verdicts[i].Error)
					require.NotNil(t, verdicts[i].Result)
					require.Equal(t, i == 0, verdicts[i].Result.Allowed)
					require.Nil(t, verdicts[i].Result.HoldExpiresAt)
					require.Zero(t, verdicts[i].Result.RetryAfterSeconds)
				} else {
					require.Nil(t, verdicts[i].Result)
					require.NotNil(t, verdicts[i].Error)
					require.Equal(t, "invalid_param", verdicts[i].Error.Code)
				}
			}
			require.Empty(t, verdicts[0].Result.BlockedBy)
			require.Empty(t, verdicts[0].Result.DenyCode)
			require.Equal(t, "money", verdicts[1].Result.BlockedBy)
			require.Equal(t, "insufficient_balance", verdicts[1].Result.DenyCode)
			require.NoError(t, client.Release(ctx, batchKey))
			require.NoError(t, client.RecordUsage(ctx, openrails.UsageReport{
				CustomerID: payer, Invoker: "client-contract", Currency: "USD", EventType: "sdk-reference-proof",
				Source: "conformance", SourceID: uuid.NewString(),
			}))
			checkClientPaymentReads(t, ctx, h, d)
			t.Run("metering", func(t *testing.T) { checkClientMetering(t, ctx, h, d); checkClientGaugeUsage(t, ctx, h, d) })
			t.Run("invoice", func(t *testing.T) { checkClientInvoice(t, ctx, h, d) })
			checkClientSpendDelegations(t, ctx, h, d)
			checkClientAdmissionFields(t, ctx, h, d)
			checkClientSettings(t, ctx, h, d)
		})
	}
}

func checkClientMetering(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client, mid := d.client, d.mid
	mctx := merchant.WithID(ctx, mid)
	product, payer := uuid.New(), uuid.New()
	key := "meter-client-" + uuid.NewString()
	_, err := h.Pool().Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Usage product')`, product, mid.UUID(), key)
	require.NoError(t, err)
	spec := openrails.UsageMeterSpec{Key: key, EventType: key, Aggregation: "sum", ValueProperty: "units", Unit: "requests"}
	require.NoError(t, client.EnsureUsageMeter(ctx, spec))
	first, err := client.GetUsageMeter(ctx, key)
	require.NoError(t, err)
	require.NoError(t, client.EnsureUsageMeter(ctx, spec))
	again, err := client.GetUsageMeter(ctx, key)
	require.NoError(t, err)
	require.True(t, first.UpdatedAt.Equal(again.UpdatedAt))
	card, err := client.SetDefaultUsageRateCard(ctx, key, openrails.DefaultUsageRateCardRequest{ProductID: (openrails.ProductID(product)).String(), Price: pricing.RatePrice{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: 100, DivideBy: 1}}})
	require.NoError(t, err)
	require.NotNil(t, card.DefaultRateCard)
	require.EqualValues(t, 100, card.DefaultRateCard.Price.PerUnit.UnitAmount)
	ms := d.runtime.MoneyService
	mode := money.BillingModeArrears
	_, err = ms.UpsertAccountSettings(mctx, identity.CustomerID(payer), "USD", money.AccountSettingsInput{BillingMode: &mode})
	require.NoError(t, err)
	event := openrails.UsageReport{CustomerID: (openrails.CustomerID(payer)).String(), Currency: "USD", Invoker: "host", EventType: key, Dimensions: map[string]int64{"units": 3}, Source: "workflow", SourceID: uuid.NewString()}
	require.NoError(t, client.RecordUsage(ctx, event))
	require.NoError(t, client.RecordUsage(ctx, event))
	// Drive the same close used by the invoice worker, then read with the client.
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	invoice, err := ms.FinalizeInvoice(mctx, identity.CustomerID(payer), "USD", from, to)
	require.NoError(t, err)
	read, err := client.GetMerchantInvoice(ctx, invoice.ID)
	require.NoError(t, err)
	require.EqualValues(t, 300, read.AmountDue)
	replay, err := ms.FinalizeInvoice(mctx, identity.CustomerID(payer), "USD", from, to)
	require.NoError(t, err)
	require.Equal(t, invoice.ID, replay.ID)
	meters, total, err := client.ListUsageMeters(ctx, openrails.PageOptions{Limit: 100})
	require.NoError(t, err)
	require.Positive(t, total)
	require.NotEmpty(t, meters)
}

func checkClientInvoice(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client := d.client
	errorLogs := &errorLogHook{}
	logger := logrus.StandardLogger()
	previousHooks := logger.ReplaceHooks(logrus.LevelHooks{})
	logger.AddHook(errorLogs)
	defer logger.ReplaceHooks(previousHooks)
	payer := identity.CustomerID(uuid.New())
	mid := d.mid
	scoped := merchant.WithID(ctx, mid)
	rt := d.runtime
	mode := money.BillingModeArrears
	require.NoError(t, rt.DB.RunInMerchantConn(scoped, func(c context.Context) error {
		_, err := rt.MoneyService.UpsertAccountSettings(c, payer, "USD", money.AccountSettingsInput{BillingMode: &mode})
		return err
	}))
	profile := openrails.InvoiceProfileDTO{NetTermsDays: 7, CollectionMethod: "send_invoice", Memo: "defaults"}
	created, err := client.EnsureCustomerInvoiceProfile(ctx, (openrails.CustomerID(payer)).String(), profile)
	require.NoError(t, err)
	require.True(t, created)
	profile.Memo = "operator settings"
	require.NoError(t, client.SetCustomerInvoiceProfile(ctx, (openrails.CustomerID(payer)).String(), profile))
	// An idempotent ensure that finds the profile is a success for the
	// caller and must not be logged by the engine as an error.
	errorLogs.drain()
	created, err = client.EnsureCustomerInvoiceProfile(ctx, (openrails.CustomerID(payer)).String(), openrails.InvoiceProfileDTO{NetTermsDays: 30, CollectionMethod: "send_invoice"})
	require.NoError(t, err)
	require.False(t, created)
	require.Empty(t, errorLogs.drain(), "idempotent EnsureCustomerInvoiceProfile logged an error")
	got, err := client.GetCustomerInvoiceProfile(ctx, (openrails.CustomerID(payer)).String())
	require.NoError(t, err)
	require.Equal(t, profile, *got)
	// Seed a real receivable via the money engine, then exercise the public
	// invoice administration workflow entirely through the common Client.
	_, err = rt.MoneyService.AccrueOwed(scoped, payer, "USD", "invoice-client", uuid.NewString(), 500)
	require.NoError(t, err)
	invoice, err := rt.MoneyService.FinalizeInvoice(scoped, payer, "USD", time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
	require.NoError(t, err)
	listed, total, err := client.ListMerchantInvoices(ctx, openrails.MerchantInvoiceFilter{CustomerID: (openrails.CustomerID(payer)).String()}, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, listed, 1)
	require.Equal(t, invoice.ID, listed[0].ID)
	require.EqualValues(t, 500, listed[0].AmountDue)
	read, err := client.GetMerchantInvoice(ctx, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, invoice.ID, read.ID)
	require.Contains(t, read.AvailableActions, openrails.InvoiceAdminRecordPayment)
	paid, err := client.RecordInvoicePayment(ctx, invoice.ID, openrails.RecordInvoicePaymentRequest{Amount: 200, Reference: "bank-transfer"})
	require.NoError(t, err)
	require.EqualValues(t, 300, paid.AmountDue)
	_, err = client.RecordInvoicePayment(ctx, invoice.ID, openrails.RecordInvoicePaymentRequest{Amount: 200, Reference: "bank-transfer"})
	require.ErrorIs(t, err, openrails.ErrConflict)
	requireCode(t, err, openrails.CodeInvoicePaymentReferenceUsed)
	_, err = client.RecordInvoicePayment(ctx, invoice.ID, openrails.RecordInvoicePaymentRequest{Amount: 10_000, Reference: "overpayment"})
	requireCode(t, err, openrails.CodeInvoicePaymentExceedsDue)
	_, err = client.RecordInvoicePayment(ctx, invoice.ID, openrails.RecordInvoicePaymentRequest{Amount: 1})
	require.ErrorIs(t, err, openrails.ErrInvalid)
	attempts, _, err := client.ListInvoicePaymentAttempts(ctx, invoice.ID, 10, 0)
	require.NoError(t, err)
	require.NotNil(t, attempts)
	voided, err := client.VoidInvoice(ctx, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, "voided", voided.Status)
	_, err = client.RecordInvoicePayment(ctx, invoice.ID, openrails.RecordInvoicePaymentRequest{Amount: 1, Reference: "after-void"})
	require.ErrorIs(t, err, openrails.ErrConflict)
	_, err = client.GetMerchantInvoice(ctx, uuid.New())
	require.ErrorIs(t, err, openrails.ErrNotFound)
}

func checkClientGaugeUsage(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client, pool, currency := d.client, h.MerchantPool(d.mid.UUID()), money.DefaultCurrency
	merchantID := d.mid.UUID()
	meterKey := "gb-seconds-" + uuid.NewString()
	productID := uuid.New()
	payerID := uuid.New()
	payer := identity.CustomerID(payerID)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.usage_events WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoice_items WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoices WHERE customer_id = $1", payerID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.catalog_rate_cards WHERE merchant_id = $1 AND product_id = $2", merchantID, productID)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.catalog_meters WHERE key = $1", meterKey)
		_, _ = pool.Exec(ctx, "DELETE FROM billing.products WHERE id = $1", productID)
	})

	// Arrears so rated usage becomes an open receivable.
	svc, err := service.New(d.runtime)
	require.NoError(t, err)
	mode := money.BillingModeArrears
	require.NoError(t, svc.SetCreditAccountSettings(merchant.WithID(ctx, d.mid), payer, currency,
		money.AccountSettingsInput{BillingMode: &mode}))

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	// Sub-second instant: the wire carries RFC3339 with fractional seconds, so
	// the event lands at exactly this microsecond on both transports.
	occurred := time.Now().UTC().Truncate(time.Microsecond)
	if occurred.Nanosecond() == 0 {
		occurred = occurred.Add(time.Microsecond)
	}

	// Two gauge segment events via the EMBEDDED unified client; the first is
	// replayed and must not double-record.
	report := openrails.UsageReport{
		CustomerID: (openrails.CustomerID(payerID)).String(),
		Invoker:    "usage-report-test",
		Currency:   currency,
		EventType:  meterKey,
		Dimensions: map[string]int64{meterKey: 1800},
		Amount:     0,
		Source:     "usage-report-test",
		SourceID:   uuid.NewString(),
		OccurredAt: &occurred,
	}
	require.NoError(t, client.RecordUsage(ctx, report))
	require.NoError(t, client.RecordUsage(ctx, report), "idempotent replay must succeed")
	second := report
	second.SourceID = uuid.NewString()
	second.Dimensions = map[string]int64{meterKey: 5400}
	require.NoError(t, client.RecordUsage(ctx, second))

	// One more via the STANDALONE client (same wire, real HTTP + AuthKit).
	third := report
	third.SourceID = uuid.NewString()
	third.Dimensions = map[string]int64{meterKey: 3600}
	require.NoError(t, client.RecordUsage(ctx, third))

	var eventCount int
	var totalUnits int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM((dimensions ->> $2)::bigint), 0)
		FROM billing.usage_events
		WHERE customer_id = $1 AND event_type = $2`, payerID, meterKey).Scan(&eventCount, &totalUnits))
	require.Equal(t, 3, eventCount, "replay must not create a fourth event")
	require.Equal(t, int64(1800+5400+3600), totalUnits)
	var occurredAt []time.Time
	rows, err := pool.Query(ctx, `SELECT DISTINCT occurred_at FROM billing.usage_events WHERE customer_id = $1 AND event_type = $2`, payerID, meterKey)
	require.NoError(t, err)
	for rows.Next() {
		var at time.Time
		require.NoError(t, rows.Scan(&at))
		occurredAt = append(occurredAt, at.UTC())
	}
	rows.Close()
	require.Equal(t, []time.Time{occurred}, occurredAt, "both transports keep the sub-second occurred_at")
	exact, err := client.UsageRollup(ctx, (openrails.CustomerID(payerID)).String(), currency, occurred, occurred.Add(time.Microsecond), "resource")
	require.NoError(t, err)
	require.Len(t, exact, 1)
	after, err := client.UsageRollup(ctx, (openrails.CustomerID(payerID)).String(), currency, occurred.Add(time.Microsecond), occurred.Add(time.Second), "resource")
	require.NoError(t, err)
	require.Empty(t, after)

	// Gauge rate card: 500_000 micros per 3600 unit-seconds.
	rateMicros, divideBy := int64(500_000), int64(3600)
	_, err = pool.Exec(ctx, `INSERT INTO billing.products (id, key, display_name, merchant_id) VALUES ($1, $2, $3, $4)`,
		productID, "usage-report-"+uuid.NewString(), "Usage Report Product", merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO billing.catalog_meters (merchant_id, key, aggregation) VALUES ($1, $2, 'sum')`,
		merchantID, meterKey)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO billing.catalog_rate_cards (merchant_id, product_id, ordinal, meter_key, payment_term, price)
VALUES ($1, $2, 1, $3, 'in_arrears', jsonb_build_object(
    'model', 'per_unit',
    'currency', 'USD',
    'per_unit', jsonb_build_object('unit_amount', $4::bigint::text, 'divide_by', $5::bigint)))`,
		merchantID, productID, meterKey, rateMicros, divideBy)
	require.NoError(t, err)

	expected, err := pricing.ChargeModel{Kind: pricing.ModelPerUnit, UnitAmount: rateMicros, DivideBy: divideBy}.Rate(totalUnits)
	require.NoError(t, err)
	require.EqualValues(t, 1_500_000, expected)

	inv, err := svc.FinalizeInvoice(merchant.WithID(ctx, d.mid), payer, currency, from, to)
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)
	require.Equal(t, expected, inv.AmountDue, "invoice must equal reported unit-seconds x rate")

	// Re-finalize the same window: idempotent (per-period watermark).
	inv2, err := svc.FinalizeInvoice(merchant.WithID(ctx, d.mid), payer, currency, from, to)
	require.NoError(t, err)
	require.Equal(t, inv.ID, inv2.ID)
	require.Equal(t, expected, inv2.AmountDue)
}

func checkClientSettings(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	local, remote := d.client, d.peer
	clients := []*openrails.Client{local, remote}
	amount, days, email := int64(500_000), 9, "operator@example.test"
	routing := []openrails.CheckoutRoutingRule{{Prefer: []string{"nmi"}}}
	document := openrails.MerchantSettings{
		Profile:                    &openrails.MerchantProfileInput{DisplayName: "Conformance Billing", SignupURL: "https://example.test/signup", LogoURL: "https://example.com/logo.png", FromEmail: "billing@example.com", SupportURL: "https://example.com/support"},
		InvoiceCollectionThreshold: &amount, InvoiceMonthlyFloor: &amount, InvoiceBillingBoundary: "calendar_month",
		AlertEmail: &email, RepriceNoticeWindowDays: &days, ArrearsGraceDays: &days, ArrearsDelinquencyFloor: &amount,
		CheckoutRouting:                   &routing,
		BillingPolicies:                   []openrails.BillingPolicyInput{{Name: "gold", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10_000_000}, {Name: "conf_window", Kind: "window_spend_cap", SpendWindows: []openrails.BudgetWindowInput{{Key: "hourly", WindowSeconds: 3600, Limit: 10_000}}}},
		BillingPolicyBindings:             []openrails.BillingPolicyBindingInput{{PolicyName: "gold"}, {PolicyName: "conf_window", Tier: "conf"}},
		DelegatedInvokerWastedSpendLimits: []openrails.BudgetWindowInput{{Key: "short", WindowSeconds: 300, Limit: 500_000, Currency: "USD"}},
	}
	read := func(c *openrails.Client) (openrails.MerchantSettings, string) {
		t.Helper()
		got, err := c.GetMerchantSettings(ctx)
		require.NoError(t, err)
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		return *got, string(raw)
	}

	// A database failure after the config/schedule writes must roll everything back.
	_, err := h.Pool().Exec(ctx, `CREATE FUNCTION billing.client_workflow_fail_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.tier = 'issue999-fail' THEN RAISE EXCEPTION 'injected issue999 binding failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER client_workflow_fail_binding BEFORE INSERT ON billing.billing_policy_bindings FOR EACH ROW EXECUTE FUNCTION billing.client_workflow_fail_binding();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.Background(), `DROP TRIGGER IF EXISTS client_workflow_fail_binding ON billing.billing_policy_bindings; DROP FUNCTION IF EXISTS billing.client_workflow_fail_binding();`)
		require.NoError(t, err)
	})
	for i, client := range clients {
		t.Run([]string{"client", "peer"}[i], func(t *testing.T) {
			require.NoError(t, client.SetMerchantSettings(ctx, document))
			got, before := read(client)
			require.Equal(t, document.InvoiceCollectionThreshold, got.InvoiceCollectionThreshold)
			require.Equal(t, document.CheckoutRouting, got.CheckoutRouting)
			require.Equal(t, document.Profile, got.Profile)
			var raw []byte
			require.NoError(t, h.MerchantPool(d.mid.UUID()).QueryRow(ctx, `SELECT config FROM billing.merchant_configurations WHERE merchant_id=$1`, d.mid.UUID()).Scan(&raw))
			var persisted models.MerchantConfiguration
			require.NoError(t, json.Unmarshal(raw, &persisted))
			require.Equal(t, document.Profile.DisplayName, persisted.Profile.DisplayName)
			require.Equal(t, document.Profile.LogoURL, persisted.Profile.LogoURL)
			require.Equal(t, document.Profile.FromEmail, persisted.Profile.FromEmail)
			require.Equal(t, document.Profile.SupportURL, persisted.Profile.SupportURL)
			require.Len(t, got.DelegatedInvokerWastedSpendLimits, 1)
			require.Equal(t, document.DelegatedInvokerWastedSpendLimits, got.DelegatedInvokerWastedSpendLimits)
			require.NoError(t, client.SetMerchantSettings(ctx, got))
			_, after := read(client)
			require.JSONEq(t, before, after)

			bad := got
			bad.Profile = &openrails.MerchantProfileInput{DisplayName: "must not commit"}
			bad.BillingPolicyBindings = []openrails.BillingPolicyBindingInput{{PolicyName: "missing"}}
			require.ErrorIs(t, client.SetMerchantSettings(ctx, bad), openrails.ErrInvalid)
			_, after = read(client)
			require.JSONEq(t, before, after)
			bad.BillingPolicyBindings = []openrails.BillingPolicyBindingInput{{PolicyName: "gold", Tier: "issue999-fail"}}
			require.ErrorIs(t, client.SetMerchantSettings(ctx, bad), openrails.ErrInternal)
			_, after = read(client)
			require.JSONEq(t, before, after)
		})
	}

	// A second runtime must see both tightening and loosening immediately.
	payer := openrails.CustomerID(uuid.New())
	_, err = local.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: new(payer.String()), Invoker: "issue999", Currency: "USD", Amount: 20_000_000, Source: "issue999", SourceID: uuid.NewString()})
	require.NoError(t, err)
	admit := func(expected bool) {
		t.Helper()
		deadline := time.Now().Add(time.Hour)
		requestID := uuid.NewString()
		result, err := remote.Admit(ctx, openrails.AdmitRequest{CustomerID: (openrails.CustomerID(payer)).String(), Invoker: "issue999", InvokerType: "payer", Currency: "USD", Source: "issue999", RequestID: requestID, ExpiresAt: &deadline, AccrualRateDeltaPerHour: 11_000_000, EstimatedAmount: 1000})
		require.NoError(t, err)
		require.Equal(t, expected, result.Allowed)
		if result.Allowed {
			require.NoError(t, remote.Release(ctx, requestID))
		}
	}
	admit(false)
	document.BillingPolicies[0].AccrualRateCapPerHour = 12_000_000
	require.NoError(t, local.SetMerchantSettings(ctx, document))
	admit(true)
	document.BillingPolicies[0].AccrualRateCapPerHour = 10_000_000
	require.NoError(t, local.SetMerchantSettings(ctx, document))
	admit(false)

	// Runtime customer bindings survive document roundtrips and prevent removal
	// of a policy they still reference, even when its FK is configured to cascade.
	_, err = h.Pool().Exec(ctx, `INSERT INTO billing.billing_policy_bindings(id,merchant_id,customer_id,policy_name,created_at,updated_at) VALUES($1,$2,$3,'gold',now(),now())`, uuid.New(), d.mid.UUID(), payer.UUID())
	require.NoError(t, err)
	got, before := read(remote)
	require.NoError(t, remote.SetMerchantSettings(ctx, got))
	require.ErrorIs(t, remote.SetMerchantSettings(ctx, openrails.MerchantSettings{}), openrails.ErrInvalid)
	_, after := read(remote)
	require.JSONEq(t, before, after)
}

func checkClientSpendDelegations(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client := d.client
	customerID := uuid.New()
	_, err := client.EnsureCustomer(ctx, (openrails.CustomerID(customerID)).String())
	require.NoError(t, err)
	svc, err := service.New(d.runtime)
	require.NoError(t, err)
	err = client.SetCustomerSpendDelegations(ctx, (openrails.CustomerID(customerID)).String(), []openrails.SpendDelegationInput{
		{
			Scope:    "invoker",
			ScopeKey: "test-invoker",
			Windows:  []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"}},
		},
		{
			Scope:    "role",
			ScopeKey: "test-role",
			Windows: []openrails.SpendLimitWindow{{
				Key: "month", WindowSeconds: 2592000, Limit: 9_000_000, Currency: "USD",
			}},
		},
	})
	require.NoError(t, err, "embedded SetCustomerSpendDelegations must pin the bound merchant itself")

	require.NoError(t, client.SetCustomerSpendDelegation(ctx, (openrails.CustomerID(customerID)).String(), openrails.SpendDelegationInput{
		Scope:    "invoker",
		ScopeKey: "test-invoker",
		// Currency is intentionally omitted: spend limits are also valid for
		// non-monetary units, and the singular upsert must preserve that contract.
		Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 123}},
	}))
	stored, err := svc.InvokerSpendLimits(merchant.WithID(ctx, d.mid), identity.CustomerID(customerID))
	require.NoError(t, err)
	require.Len(t, stored, 2, "single embedded upsert must preserve unrelated rows")
	limits := map[string]int64{}
	for _, row := range stored {
		limits[row.Scope+"\x00"+row.ScopeKey] = row.Windows[0].Limit
	}
	require.EqualValues(t, 123, limits["invoker\x00test-invoker"])
	require.EqualValues(t, 9_000_000, limits["role\x00test-role"])

	err = client.SetCustomerSpendDelegations(ctx, (openrails.CustomerID(customerID)).String(), []openrails.SpendDelegationInput{
		{
			Scope: " role ", ScopeKey: " test-role ",
			Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 1}},
		},
		{
			Scope: "role", ScopeKey: "test-role",
			Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 2}},
		},
	})
	require.ErrorIs(t, err, openrails.ErrInvalid)
	var embeddedStatus *openrails.StatusError
	require.ErrorAs(t, err, &embeddedStatus)
	require.Equal(t, 400, embeddedStatus.Status)
	require.Contains(t, err.Error(), "duplicate delegation for role")
	stored, err = svc.InvokerSpendLimits(merchant.WithID(ctx, d.mid), identity.CustomerID(customerID))
	require.NoError(t, err)
	require.Len(t, stored, 2, "rejected embedded duplicate document must not mutate policy")

	// Replace-with-empty exercises the delete lane through the same ctx path.
	require.NoError(t, client.SetCustomerSpendDelegations(ctx, (openrails.CustomerID(customerID)).String(), nil))
}

func checkClientAdmissionFields(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client := d.client
	require.NoError(t, client.SetMerchantSettings(ctx, openrails.MerchantSettings{
		BillingPolicies:       []openrails.BillingPolicyInput{{Name: "client_rate", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10_000_000}},
		BillingPolicyBindings: []openrails.BillingPolicyBindingInput{{PolicyName: "client_rate"}},
	}))
	payer := uuid.New()
	_, err := h.Pool().Exec(ctx, `INSERT INTO billing.customers
		(id, merchant_id, issuer, created_at, last_seen_at)
		VALUES ($1, $2, 'client-fields', now(), now())`, payer, d.mid.UUID())
	require.NoError(t, err)
	id := openrails.CustomerID(payer)
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: new(id.String()), Invoker: "client-fields", Currency: "USD", Amount: 1_000_000,
		Source: "client-fields", SourceID: uuid.NewString(),
	})
	require.NoError(t, err)
	expires := time.Now().Add(time.Hour)
	req := openrails.AdmitRequest{
		CustomerID: (openrails.CustomerID(payer)).String(), Invoker: "client-fields", InvokerType: "payer", Currency: "USD",
		EstimatedAmount: 1000, ExpiresAt: &expires, AccrualRateDeltaPerHour: 11_000_000,
		RequestID: uuid.NewString(), Source: "client-fields", Resource: "compute", TrustLevel: "standard",
	}
	verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{req})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.NotNil(t, verdicts[0].Result)
	require.False(t, verdicts[0].Result.Allowed)
	require.Equal(t, "accrual_rate_cap_reached", verdicts[0].Result.DenyCode)
	{
		single := client
		verdict, err := single.Admit(ctx, req)
		require.NoError(t, err)
		require.False(t, verdict.Allowed)
		require.Equal(t, "accrual_rate_cap_reached", verdict.DenyCode)
	}
	req.AccrualRateDeltaPerHour = 1_000_000
	verdicts, err = client.AdmitBatch(ctx, []openrails.AdmitRequest{req})
	require.NoError(t, err)
	require.True(t, verdicts[0].Result.Allowed)
	require.NoError(t, client.Release(ctx, req.RequestID))

	grant := openrails.SpendDelegationInput{
		Scope: "invoker", ScopeKey: "client-fields", Provenance: "original-policy",
		Windows: []openrails.SpendLimitWindow{{Key: "5h", WindowSeconds: 18000, Limit: 1_000_000, Currency: "USD"}},
	}
	readProvenance := func() string {
		t.Helper()
		var value string
		require.NoError(t, h.Pool().QueryRow(ctx, `SELECT provenance FROM billing.invoker_spend_limits
			WHERE merchant_id=$1 AND customer_id=$2 AND scope=$3 AND scope_key=$4`,
			d.mid.UUID(), payer, grant.Scope, grant.ScopeKey).Scan(&value))
		return value
	}
	require.NoError(t, client.SetCustomerSpendDelegations(ctx, (openrails.CustomerID(payer)).String(), []openrails.SpendDelegationInput{grant}))
	require.Equal(t, grant.Provenance, readProvenance())
	grant.Provenance = "updated-policy"
	require.NoError(t, client.SetCustomerSpendDelegation(ctx, (openrails.CustomerID(payer)).String(), grant))
	require.Equal(t, grant.Provenance, readProvenance())
}

func checkClientPaymentReads(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client := d.client
	pool := h.MerchantPool(d.mid.UUID())
	payer, other, product, price, payment, refund := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) { _, err := pool.Exec(ctx, sql, args...); require.NoError(t, err) }
	exec(`INSERT INTO billing.customers (id,merchant_id) VALUES ($1,$2),($3,$2)`, payer, d.mid.UUID(), other)
	exec(`INSERT INTO billing.products (id,merchant_id,key,display_name) VALUES ($1,$2,$3,'Payment reads')`, product, d.mid.UUID(), uuid.NewString())
	exec(`INSERT INTO billing.prices (id,merchant_id,product_id,amount,currency) VALUES ($1,$2,$3,$4,'USD')`, price, d.mid.UUID(), product, int64(math.MaxInt64))
	psp := dbtest.EnsureTestPSP(ctx, t, pool, d.mid.UUID(), "nmi")
	purchased := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	exec(`INSERT INTO billing.payments (id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,purchased_at)
		VALUES ($1,$2,$3,$4,'nmi',$5,$6,$6,'USD','completed','rail',$7,$8)`, payment, d.mid.UUID(), payer, price, uuid.NewString(), int64(math.MaxInt64), psp, purchased)
	exec(`INSERT INTO billing.payments (id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,refunded_payment_id,purchased_at)
		VALUES ($1,$2,$3,$4,'nmi',$5,-1,-1,'USD','completed','rail',$6,$7,$8)`, refund, d.mid.UUID(), payer, price, uuid.NewString(), psp, payment, purchased.Add(time.Minute))
	exec(`INSERT INTO billing.payments (id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,purchased_at)
		VALUES ($1,$2,$3,$4,'nmi',$5,500,500,'USD','completed','rail',$6,$7)`, uuid.New(), d.mid.UUID(), other, price, uuid.NewString(), psp, purchased)

	got, err := client.GetPayment(ctx, openrails.PaymentID(payment))
	require.NoError(t, err)
	require.Equal(t, openrails.PaymentID(payment), got.ID)
	require.EqualValues(t, math.MaxInt64, got.Amount, "the full int64 range survives the wire")
	require.EqualValues(t, 1, got.AmountRefunded)
	require.Equal(t, "USD", got.Currency)
	require.Equal(t, payer.String(), got.CustomerID)
	require.NotNil(t, got.Price)
	require.Equal(t, openrails.PriceID(price).String(), got.Price.ID)
	require.EqualValues(t, math.MaxInt64, got.Price.UnitAmount)
	require.NotNil(t, got.Refunds)
	require.Len(t, got.Refunds.Data, 1)
	require.EqualValues(t, -1, got.Refunds.Data[0].Amount)

	page, err := client.ListPayments(ctx, openrails.PaymentFilter{CustomerID: (openrails.CustomerID(payer)).String(), PriceID: (openrails.PriceID(price)).String()})
	require.NoError(t, err)
	require.EqualValues(t, 2, page.Total, "the payer's payment and its refund")
	for _, item := range page.Data {
		require.Equal(t, payer.String(), item.CustomerID)
	}
	all, err := client.ListPayments(ctx, openrails.PaymentFilter{PageOptions: openrails.PageOptions{Limit: 1}})
	require.NoError(t, err)
	require.Len(t, all.Data, 1)
	require.EqualValues(t, 3, all.Total)
	require.True(t, all.HasMore)

	_, err = client.GetPayment(ctx, openrails.PaymentID(uuid.New()))
	require.ErrorIs(t, err, openrails.ErrNotFound)
	_, err = client.GetPayment(ctx, openrails.PaymentID{})
	require.ErrorIs(t, err, openrails.ErrInvalid)
	peer, err := d.peer.GetPayment(ctx, openrails.PaymentID(payment))
	require.NoError(t, err)
	require.Equal(t, got, peer, "independent runtime returns the identical payment")
	_, err = d.stranger.GetPayment(ctx, openrails.PaymentID(payment))
	require.ErrorIs(t, err, openrails.ErrNotFound)
}
