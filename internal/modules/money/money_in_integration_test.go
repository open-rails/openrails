//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/payments/charge"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/intents"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// moneyInEnv provisions a fresh payer with NO initial deposit (balance 0).
func moneyInEnv(t *testing.T) (*money.MoneyService, *pgxpool.Pool, identity.CustomerID, string, context.Context) {
	svc, _, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	return svc, pool, payer, currency, ctx
}

func moneyInEnvWithDB(t *testing.T) (*money.MoneyService, *db.DB, *pgxpool.Pool, identity.CustomerID, string, context.Context) {
	t.Helper()
	ctx := context.Background()

	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	ctx = dbtest.WithTestMerchant(ctx)

	payer := identity.CustomerIDFromString(uuid.NewString())
	payerID := payer.UUID()
	t.Cleanup(func() {
		// money_spend_limits (#513 B1), money_blocks + money_transactions (#512)
		// were dropped; only money_settings remains to reset.
		_, _ = pool.Exec(ctx, "DELETE FROM billing.money_settings WHERE customer_id = $1", payerID)
	})
	return money.NewMoneyService(dbi), dbi, pool, payer, money.DefaultCurrency, ctx
}

// --- fakes ---

// fakeCharger scripts the provider boundary. Prepare failures never reach the
// provider (prepareFailures); Submit failures are possible submissions
// (submitErrors / lostResponse).
type fakeCharger struct {
	mu         sync.Mutex
	charges    []money.ChargeRequest
	declineAll bool
	// lostResponse: the charge lands but the response is lost (typed
	// transport-ambiguous error).
	lostResponse bool
	// submitErrors: an untyped error after the send for the first N submits.
	submitErrors int
	// prepareFailures: a clean pre-send failure (provider not armed) for the
	// first N prepares.
	prepareFailures int
	// landed records the operation keys whose charge executed at the "provider".
	landed map[string]string
}

func (f *fakeCharger) Prepare(_ context.Context, req money.ChargeRequest) (money.PreparedCharge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prepareFailures > 0 {
		f.prepareFailures--
		return nil, errors.New("provider offline")
	}
	return money.PreparedChargeFunc(func(context.Context) (money.ChargeResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.charges = append(f.charges, req)
		if f.declineAll {
			return money.ChargeResult{Declined: true}, nil
		}
		txn := "tx_" + req.IdempotencyKey
		if f.landed == nil {
			f.landed = map[string]string{}
		}
		f.landed[req.IdempotencyKey] = txn
		if f.submitErrors > 0 {
			f.submitErrors--
			return money.ChargeResult{}, errors.New("connection reset after send")
		}
		if f.lostResponse {
			return money.ChargeResult{}, &nmi.TransportAmbiguousError{Err: errors.New("timeout after send")}
		}
		return money.ChargeResult{TransactionID: txn, ExternalInvoiceID: "in_" + req.IdempotencyKey}, nil
	}), nil
}

func (f *fakeCharger) chargeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.charges)
}

func (f *fakeCharger) ConfirmCollectionNotExecuted(_ context.Context, in gen.OpenrailsRailIntent) error {
	return errors.New("scripted provider has no authoritative nonexecution evidence")
}

type fakeCollectionAdapter struct {
	charges         []money.ChargeRequest
	methods         []gen.OpenrailsPaymentMethod
	decline         bool
	submitErrors    int
	prepareFailures int
}

func (f *fakeCollectionAdapter) Prepare(_ context.Context, method gen.OpenrailsPaymentMethod, req money.ChargeRequest) (money.PreparedCharge, error) {
	if f.prepareFailures > 0 {
		f.prepareFailures--
		return nil, errors.New("gateway not armed")
	}
	return money.PreparedChargeFunc(func(context.Context) (money.ChargeResult, error) {
		f.methods = append(f.methods, method)
		f.charges = append(f.charges, req)
		if f.submitErrors > 0 {
			f.submitErrors--
			return money.ChargeResult{}, errors.New("gateway timeout")
		}
		if f.decline {
			code := "card_declined"
			message := "card declined"
			return money.ChargeResult{Rail: method.Rail, TransactionID: "declined_" + req.IdempotencyKey, Declined: true, FailureCode: &code, FailureMessage: &message}, nil
		}
		return money.ChargeResult{Rail: method.Rail, TransactionID: "tx_" + req.IdempotencyKey}, nil
	}), nil
}

// chargeThrough runs both charger phases, as the intent handler does.
func chargeThrough(ctx context.Context, ch money.Charger, req money.ChargeRequest) (money.ChargeResult, error) {
	prepared, err := ch.Prepare(ctx, req)
	if err != nil {
		return money.ChargeResult{}, err
	}
	return prepared.Submit(ctx)
}

// collectionRunner is the invoice_collection ledger runner the production
// invoice worker and admin retry route drive, over a scripted charger.
func collectionRunner(dbi *db.DB, charger money.Charger, verifier money.CollectionVerifier) *intents.Runner {
	return collectionRunnerWith(dbi, charger, verifier, fullModeConfig())
}

func collectionRunnerWith(dbi *db.DB, charger money.Charger, verifier money.CollectionVerifier, cfg *config.Config) *intents.Runner {
	return collectionRunnerFull(dbi, charger, verifier, cfg, nil)
}

// collectionRunnerClock shares one clock between the handler's schedule
// decisions and the runner's lease/claim reads.
func collectionRunnerClock(dbi *db.DB, charger money.Charger, verifier money.CollectionVerifier, clock clockwork.Clock) *intents.Runner {
	return collectionRunnerFull(dbi, charger, verifier, fullModeConfig(), clock)
}

func collectionRunnerFull(dbi *db.DB, charger money.Charger, verifier money.CollectionVerifier, cfg *config.Config, clock clockwork.Clock) *intents.Runner {
	if verifier == nil {
		verifier, _ = charger.(money.CollectionVerifier)
	}
	return &intents.Runner{
		Store:    intents.NewStore(dbi),
		Registry: intents.NewRegistry(money.NewInvoiceCollectionHandler(dbi, charger, verifier, cfg, clock)),
		Config:   cfg,
		Clock:    clock,
	}
}

// collectionIntents lists the invoice's collection operations, oldest first.
func collectionIntents(t *testing.T, pool *pgxpool.Pool, ctx context.Context, invoiceID uuid.UUID) []gen.OpenrailsRailIntent {
	t.Helper()
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	rows, err := pool.Query(ctx, `SELECT id FROM billing.rail_intents WHERE merchant_id=$1 AND intent_type = $2 AND payload->>'invoice_id' = $3 ORDER BY created_at, id`, mid.UUID(), money.TypeInvoiceCollection, invoiceID.String())
	require.NoError(t, err)
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]gen.OpenrailsRailIntent, 0, len(ids))
	for _, id := range ids {
		row, err := dbtest.Queries(pool).GetRailIntent(ctx, gen.GetRailIntentParams{MerchantID: mid.UUID(), ID: id})
		require.NoError(t, err)
		out = append(out, row)
	}
	return out
}

func latestCollectionIntent(t *testing.T, pool *pgxpool.Pool, ctx context.Context, invoiceID uuid.UUID) gen.OpenrailsRailIntent {
	t.Helper()
	all := collectionIntents(t, pool, ctx, invoiceID)
	require.NotEmpty(t, all, "no collection operation for invoice %s", invoiceID)
	return all[len(all)-1]
}

func dueNow(t *testing.T, pool *pgxpool.Pool, ctx context.Context, intentID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx, "UPDATE billing.rail_intents SET next_attempt_at = now() WHERE id = $1", intentID)
	require.NoError(t, err)
}

func cleanupCollection(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "UPDATE billing.invoices SET collection_intent_id = NULL WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.rail_mutation_logs WHERE rail_intent_id IN (SELECT id FROM billing.rail_intents WHERE intent_type = $1 AND (payload->>'customer_id' = $2 OR payload IS NULL))", money.TypeInvoiceCollection, payer.UUID().String())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.rail_intents WHERE intent_type = $1 AND (payload->>'customer_id' = $2 OR (payload IS NULL AND merchant_id = $3))", money.TypeInvoiceCollection, payer.UUID().String(), dbtest.TestMerchantID.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.usage_events WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM billing.invoices WHERE customer_id = $1", payer.UUID())
	})
}

// latestBlockExpiry returns the expiry of the most recent credit lot (a #514
// credit grant) for the payer — the money_blocks table is gone (#512 hard cut),
// the credit grant carries the lot's amount + expiry.
func latestBlockExpiry(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payerID uuid.UUID) *time.Time {
	t.Helper()
	var exp *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT ends_at FROM billing.grants WHERE customer_id = $1 AND kind = 'credit' AND event = 'grant' ORDER BY created_at DESC LIMIT 1",
		payerID).Scan(&exp))
	return exp
}

// frozenInstrumentOf is what an operation freezes for a saved method: every
// charge states the instrument it was armed against.
func frozenInstrumentOf(t *testing.T, pool *pgxpool.Pool, ctx context.Context, pm uuid.UUID) charge.FrozenInstrument {
	t.Helper()
	row, err := dbtest.Queries(pool).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: dbtest.TestMerchantID.UUID(), ID: pm})
	require.NoError(t, err)
	return charge.FreezeInstrument(row)
}

func seedPaymentMethod(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, rail string) uuid.UUID {
	t.Helper()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payer.UUID().String())
	pm := uuid.New()
	return seedPaymentMethodRow(t, pool, ctx, payer, rail, pm, "vault_"+pm.String())
}

func seedPaymentMethodWithRailCustomerRef(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, rail, railCustomerRef string) uuid.UUID {
	t.Helper()
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payer.UUID().String())
	// One row with the requested rail + vault. (Was double-inserting the
	// same id via seedPaymentMethod first → duplicate payment_methods_pkey.)
	return seedPaymentMethodRow(t, pool, ctx, payer, rail, uuid.New(), railCustomerRef)
}

// testPSPForRail resolves an existing armed PSP for (merchant, rail) — several
// tests in this package arm a SPECIFIC account (seedPSPSecrets)
// before seeding a payment method/rail-customer row, and that row's psp_id
// must match the armed one or credential resolution 404s. Only mints a fresh
// generic PSP via dbtest.EnsureTestPSP when nothing is armed yet.
func testPSPForRail(t *testing.T, pool *pgxpool.Pool, ctx context.Context, rail string) uuid.UUID {
	t.Helper()
	var existing uuid.UUID
	err := pool.QueryRow(ctx,
		`SELECT id FROM billing.psps WHERE merchant_id = $1 AND rail = $2 AND archived = false
		 ORDER BY created_at DESC LIMIT 1`,
		dbtest.TestMerchantID.UUID(), rail).Scan(&existing)
	if err == nil {
		return existing
	}
	require.ErrorIs(t, err, pgx.ErrNoRows)
	return dbtest.EnsureTestPSP(ctx, t, pool, dbtest.TestMerchantID.UUID(), rail)
}

func seedPaymentMethodRow(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, rail string, pm uuid.UUID, railCustomerRef string) uuid.UUID {
	t.Helper()
	pspID := testPSPForRail(t, pool, ctx, rail)
	params := gen.CreatePaymentMethodParams{
		ID:                   pm,
		MerchantID:           dbtest.TestMerchantID.UUID(),
		CustomerID:           payer.UUID(),
		Rail:                 rail,
		PspID:                pspID,
		InitialTransactionID: "init_" + pm.String(),
	}
	// Mirror the migration's per-rail handle placement: NMI keeps the customer
	// vault in rail_customer_ref; other rails put the instrument in rail_method_ref.
	if rails.IsNMI(models.Rail(rail)) {
		params.RailCustomerRef = railCustomerRef
	} else {
		params.RailMethodRef = railCustomerRef
	}
	_, err := dbtest.Queries(pool).CreatePaymentMethod(ctx, params)
	require.NoError(t, err)
	if rail == "stripe" {
		seedRailCustomer(t, pool, ctx, payer, rail, "cus_"+payer.UUID().String())
	}
	if rails.IsNMI(models.Rail(rail)) {
		dbtest.SeedNMIStoredCredentialRefs(ctx, t, pool, pm)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.payment_methods WHERE id = $1", pm)
	})
	return pm
}

func seedRailCustomer(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID, rail, railCustomerID string) {
	t.Helper()
	now := time.Now().UTC()
	pspID := testPSPForRail(t, pool, ctx, rail)
	require.NoError(t, dbtest.Queries(pool).UpsertRailCustomerAccount(ctx, gen.UpsertRailCustomerAccountParams{
		ID:         uuidutil.NewV7(),
		MerchantID: dbtest.TestMerchantID.UUID(),
		CustomerID: payer.UUID(),
		Rail:       rail,
		PspID:      pspID,
		AccountID:  railCustomerID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM billing.rail_customer_accounts WHERE merchant_id = $1 AND customer_id = $2 AND rail = $3", dbtest.TestMerchantID.UUID(), payer.UUID(), rail)
	})
}

func TestDepositWithoutExpiryIsPermanent(t *testing.T) {
	svc, pool, payer, _, ctx := moneyInEnv(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: money.DefaultCurrency, Amount: 1000, Source: "grant",
	})
	require.NoError(t, err)
	exp := latestBlockExpiry(t, pool, ctx, payer.UUID())
	require.Nil(t, exp, "no explicit expiry means permanent")
}

func TestScopedCharger_ValidatesPaymentMethodScopeAndDispatches(t *testing.T) {
	_, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	adapter := &fakeCollectionAdapter{}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailNMI): adapter,
	})

	frozen := frozenInstrumentOf(t, pool, ctx, pm)
	res, err := chargeThrough(ctx, ch, money.ChargeRequest{Initiator: charge.InitiatorMerchant,
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		Invoker:         payer.UUID().String(),
		PaymentMethodID: pm,
		AmountCents:     123,
		// or#864: nothing substitutes a currency before a charge — the caller
		// states it, exactly as invoice collection does in production.
		Currency:       money.DefaultCurrency,
		IdempotencyKey: "scope-ok",
		Description:    "invoice",
		Instrument:     frozen,
	})
	require.NoError(t, err)
	require.Equal(t, string(models.RailNMI), res.Rail)
	require.Equal(t, "tx_scope-ok", res.TransactionID)
	require.Len(t, adapter.charges, 1)
	require.Equal(t, pm, adapter.charges[0].PaymentMethodID)

	otherPayer := identity.CustomerIDFromString(uuid.NewString())
	_, err = ch.Prepare(ctx, money.ChargeRequest{Initiator: charge.InitiatorMerchant,
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           otherPayer,
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "wrong-customer",
	})
	require.ErrorContains(t, err, "another customer")
	require.Len(t, adapter.charges, 1, "scope failure must not dispatch")

	_, err = ch.Prepare(ctx, money.ChargeRequest{Initiator: charge.InitiatorMerchant,
		MerchantID:      uuid.New(),
		Payer:           payer,
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "wrong-merchant",
	})
	require.ErrorContains(t, err, "another merchant")
	require.Len(t, adapter.charges, 1, "merchant scope failure must not dispatch")

	// or#297/#990: the instrument moved (a custody remap, a re-vault) after
	// the operation froze it. The charge is refused BEFORE the provider.
	moved := frozen
	moved.RailCustomerRef = "vault_moved"
	_, err = ch.Prepare(ctx, money.ChargeRequest{Initiator: charge.InitiatorMerchant,
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "instrument-moved",
		Instrument:      moved,
	})
	require.ErrorIs(t, err, charge.ErrInstrumentChanged)
	require.Len(t, adapter.charges, 1, "a changed instrument must not dispatch")

	_, err = ch.Prepare(ctx, money.ChargeRequest{Initiator: charge.InitiatorMerchant,
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "no-instrument",
	})
	require.ErrorContains(t, err, "frozen instrument", "a charge must state the instrument it was armed against")
	require.Len(t, adapter.charges, 1)
}

func TestScopedCharger_RejectsUnsupportedRail(t *testing.T) {
	_, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailCCBill))
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailCCBill): &fakeCollectionAdapter{},
	})

	_, err := ch.Prepare(ctx, money.ChargeRequest{Initiator: charge.InitiatorMerchant,
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "ccbill",
		Instrument:      frozenInstrumentOf(t, pool, ctx, pm),
	})
	require.ErrorContains(t, err, "does not support invoice collection")
}

func TestScopedCharger_NMIAdapterCollectsThroughGateway(t *testing.T) {
	_, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	seen := make(chan struct{}, 1)
	// #297: collections are merchant-initiated stored-credential charges and
	// ride classic Direct Post (the portal-documented CoF lane), not v5.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "sale", r.Form.Get("type"))
		require.Equal(t, "test-security-key", r.Form.Get("security_key"))
		require.Equal(t, "vault_"+pm.String(), r.Form.Get("customer_vault_id"))
		require.Equal(t, "1.23", r.Form.Get("amount"))
		require.Equal(t, money.DefaultCurrency, r.Form.Get("currency"))
		require.Equal(t, "nmi-scope-ok", r.Form.Get("orderid"))
		require.Equal(t, "invoice", r.Form.Get("order_description"))
		require.Equal(t, "merchant", r.Form.Get("initiated_by"))
		require.Equal(t, "used", r.Form.Get("stored_credential_indicator"))
		seen <- struct{}{}
		_, _ = w.Write([]byte("response=1&responsetext=SUCCESS&authcode=OK&transactionid=txn_nmi_invoice_123&response_code=100"))
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), string(models.RailNMI), &config.NMIProviderSettings{SecurityKey: "test-security-key"}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	ch := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailNMI): client,
	}))

	res, err := chargeThrough(ctx, ch, money.ChargeRequest{Initiator: charge.InitiatorMerchant,
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		Invoker:         payer.UUID().String(),
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "nmi-scope-ok",
		Description:     "invoice",
		Instrument:      frozenInstrumentOf(t, pool, ctx, pm),
	})
	require.NoError(t, err)
	require.Equal(t, string(models.RailNMI), res.Rail)
	require.Equal(t, "txn_nmi_invoice_123", res.TransactionID)
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("NMI gateway was not called")
	}
}

func TestScopedCharger_NMIAdapterDeclineReturnsStructuredFailure(t *testing.T) {
	_, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("response=2&responsetext=Do not honor&response_code=201"))
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), string(models.RailNMI), &config.NMIProviderSettings{SecurityKey: "test-security-key"}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	ch := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailNMI): client,
	}))

	res, err := chargeThrough(ctx, ch, money.ChargeRequest{Initiator: charge.InitiatorMerchant,
		MerchantID:      dbtest.TestMerchantID.UUID(),
		Payer:           payer,
		PaymentMethodID: pm,
		AmountCents:     123,
		Currency:        money.DefaultCurrency,
		IdempotencyKey:  "nmi-decline",
		Instrument:      frozenInstrumentOf(t, pool, ctx, pm),
	})
	require.NoError(t, err)
	require.True(t, res.Declined)
	require.Equal(t, string(models.RailNMI), res.Rail)
	require.NotNil(t, res.FailureCode)
	require.Equal(t, "do_not_honor", *res.FailureCode)
	require.NotNil(t, res.FailureMessage)
	require.Contains(t, *res.FailureMessage, "Do not honor")
}

func TestChargeOutstanding_WithNMIAdapter_SettlesInvoiceThroughGateway(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, money.DefaultCurrency, pm))
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 50_000))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "nmi-invoice-collection", 50_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	seen := make(chan string, 1)
	chargedOrder := ""
	// #297: the invoice collection is a merchant-initiated stored-credential
	// charge on classic Direct Post.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			require.Equal(t, "test-security-key", r.Header.Get("Authorization"))
			require.Equal(t, "/payments/txn_nmi_invoice_settled", r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "txn_nmi_invoice_settled", "object": "transaction", "response": "1", "amount": "0.05", "currency": "USD", "customer_vault_id": "vault_" + pm.String(), "actions": []map[string]any{{"type": "sale", "amount": "0.05", "success": true}}})
			return
		}
		require.NoError(t, r.ParseForm())
		if r.Form.Get("order_id") != "" {
			require.Equal(t, chargedOrder, r.Form.Get("order_id"))
			fmt.Fprintf(w, `<nm_response><transaction><transaction_id>txn_nmi_invoice_settled</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, chargedOrder)
			return
		}

		require.NoError(t, r.ParseForm())
		require.Equal(t, "sale", r.Form.Get("type"))
		require.Equal(t, "vault_"+pm.String(), r.Form.Get("customer_vault_id"))
		require.Equal(t, "0.05", r.Form.Get("amount"))
		require.Equal(t, "merchant", r.Form.Get("initiated_by"))
		require.Equal(t, "used", r.Form.Get("stored_credential_indicator"))
		chargedOrder = r.Form.Get("orderid")
		seen <- chargedOrder
		_, _ = w.Write([]byte("response=1&responsetext=SUCCESS&authcode=OK&transactionid=txn_nmi_invoice_settled&response_code=100"))
	}))
	t.Cleanup(server.Close)

	instrument := frozenInstrumentOf(t, pool, ctx, pm)
	client, err := nmi.NewAccountClient(dbtest.TestMerchantID.UUID(), instrument.PSPID, string(models.RailNMI), &config.NMIProviderSettings{SecurityKey: "test-security-key"}, false)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL
	ch := money.NewScopedCharger(dbi, money.NewNMICollectionAdapters(map[string]*nmi.NMIClient{
		string(models.RailNMI): client,
	}))

	n, err := svc.ChargeOutstanding(ctx, collectionRunner(dbi, ch, standaloneCollectionReader{nmi: receiptFixtureNMI{client: client, request: money.ChargeRequest{Initiator: charge.InitiatorMerchant, MerchantID: dbtest.TestMerchantID.UUID(), Instrument: instrument}}}), 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	select {
	case orderID := <-seen:
		// The operation id is the provider identity: stable across every
		// execution of this operation, unique per operation.
		require.Equal(t, latestCollectionIntent(t, pool, ctx, inv.ID).ID.String(), orderID)
	case <-time.After(time.Second):
		t.Fatal("NMI gateway was not called")
	}

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(50_000), paid.AmountPaid)
	require.Equal(t, int64(0), paid.AmountDue)
	require.Nil(t, paid.CollectionIntentID, "settled operation releases the invoice")

	var railPaymentID string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(rail_payment_id), '')
		FROM billing.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, inv.ID).Scan(&railPaymentID))
	require.Equal(t, "txn_nmi_invoice_settled", railPaymentID)
}

func TestChargeOutstanding_WithScopedCharger_PrepareFailureParksWithoutProviderTraffic(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, money.DefaultCurrency, pm))
	require.NoError(t, svc.SetCreditLimit(ctx, payer, money.DefaultCurrency, 500))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "scoped-transient", 500)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	adapter := &fakeCollectionAdapter{prepareFailures: 1}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailNMI): adapter,
	})
	runner := collectionRunner(dbi, ch, adapter)
	n, err := svc.ChargeOutstanding(ctx, runner, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, adapter.charges, "a pre-send failure never reaches the provider")

	// The operation is parked (pending) without consuming an attempt; the
	// invoice stays claimed by it, so no second operation is minted.
	op := latestCollectionIntent(t, pool, ctx, inv.ID)
	require.Equal(t, intents.StatusPending, op.Status)
	require.Contains(t, *op.LastFailureReason, "gateway not armed")
	claimed, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, op.ID, *claimed.CollectionIntentID)
	n, err = svc.ChargeOutstanding(ctx, runner, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, collectionIntents(t, pool, ctx, inv.ID), 1)

	// The scheduled executor drains the parked operation once armed.
	dueNow(t, pool, ctx, op.ID)
	_, err = runner.RunExecuteOnce(ctx)
	require.NoError(t, err)
	require.Len(t, adapter.charges, 1)
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, pool, ctx, inv.ID).Status)

	var settled int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM billing.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'
	`, inv.ID).Scan(&settled))
	require.Equal(t, 1, settled)
	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
}

func TestInvoiceWorker_UsesMerchantInvoiceThresholds(t *testing.T) {
	svc, dbi, pool, payer, _, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, money.DefaultCurrency, pm))
	_, err = svc.AccrueOwed(ctx, payer, money.DefaultCurrency, "usage", "worker-scoped-collection", 500)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	collectionThreshold := int64(1_000)
	monthlyFloor := int64(100)
	require.NoError(t, merchantconfig.NewStore(dbi).Upsert(ctx, models.MerchantConfiguration{
		InvoiceCollectionThreshold: &collectionThreshold,
		InvoiceMonthlyFloor:        &monthlyFloor,
		InvoiceBillingBoundary:     money.InvoiceBoundaryCalendarMonth,
	}))

	adapter := &fakeCollectionAdapter{}
	ch := money.NewScopedCharger(dbi, map[string]money.CollectionAdapter{
		string(models.RailNMI): adapter,
	})
	// Count only this test's invoice: the worker now fans out over every active
	// merchant in the shared DB (#673).
	chargesForInvoice := func() int {
		n := 0
		for _, c := range adapter.charges {
			if c.InvoiceID != nil && *c.InvoiceID == inv.ID {
				n++
			}
		}
		return n
	}
	runner := collectionRunner(dbi, ch, adapter)
	err = riverjobs.InvoiceWorker{DB: dbi, Money: svc, Intents: runner}.Work(ctx, &river.Job[riverjobs.InvoiceArgs]{
		Args: riverjobs.InvoiceArgs{Collect: true},
	})
	require.NoError(t, err)
	require.Zero(t, chargesForInvoice(), "custom collection threshold skips the smaller invoice")

	err = riverjobs.InvoiceWorker{DB: dbi, Money: svc, Intents: runner}.Work(ctx, &river.Job[riverjobs.InvoiceArgs]{
		Args: riverjobs.InvoiceArgs{Collect: true, UseMonthlyFloor: true},
	})
	require.NoError(t, err)
	require.Equal(t, 1, chargesForInvoice())

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, int64(0), paid.AmountDue)
}
