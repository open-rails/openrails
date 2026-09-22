//go:build integration

package merchantarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// These workflows seed only merchant/customer/catalog/provider prerequisites.
// Payment, subscription, entitlement, grant and host-event rows come from the
// ordinary purchase/lifecycle writers, never from seedBook or financial SQL.
func TestRegisteredPurchaseArchiveRoundTrip(t *testing.T) {
	testPurchaseWorkflowArchive(t, false, "")
}

func TestRegisteredSubscriptionArchiveRoundTrip(t *testing.T) {
	testPurchaseWorkflowArchive(t, true, "")
}

type purchaseArchiveServices struct {
	payments      *payments.PaymentService
	entitlements  *entitlements.EntitlementService
	access        *productaccess.Service
	lifecycle     *subscriptions.SubscriptionLifecycleService
	subscriptions *subscriptions.SubscriptionService
}

func newPurchaseArchiveServices(d *db.DB, clock clockwork.Clock) purchaseArchiveServices {
	products := catalog.NewProductService(d)
	prices := catalog.NewPriceService(d)
	pay := payments.NewPaymentService(d, clock)
	ents := entitlements.NewEntitlementService(d, clock)
	subs := subscriptions.NewSubscriptionService(d, prices, products, nil, clock)
	access := productaccess.NewService(d, clock)
	return purchaseArchiveServices{pay, ents, access,
		subscriptions.NewSubscriptionLifecycleService(d, products, prices, ents, nil, pay, clock), subs}
}

func testPurchaseWorkflowArchive(t *testing.T, recurring bool, phase string) {
	t.Helper()
	schema, rail := "archive_purchase_writer", models.RailNMI
	if recurring {
		schema = "archive_subscription_writer"
	}
	source, target := archiveDB(t, "openrails"), archiveDB(t, schema)
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	customer, product, price, psp := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	account := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	if phase == "activated" {
		now = now.Add(-7 * 24 * time.Hour)
	}
	clock := clockwork.NewFakeClockAt(now)
	ctx, release, err := source.WithMerchantConn(merchant.WithID(t.Context(), id))
	require.NoError(t, err)
	defer release()
	ctx = db.WithPSPID(ctx, psp)
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO openrails.customers(merchant_id,id,issuer) VALUES($1,$2,'https://archive-purchase.example')`, []any{id.UUID(), customer}},
		{`INSERT INTO openrails.products(merchant_id,id,key,display_name,entitlements_spec) VALUES($1,$2,'writer-product','Writer product','{"archive_access":48,"archive_download":48}')`, []any{id.UUID(), product}},
		{`INSERT INTO openrails.prices(merchant_id,id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,'writer-price',2500000,'USD',48,$4)`, []any{id.UUID(), price, product, recurring}},
		{`INSERT INTO openrails.psps(merchant_id,id,rail,environment,account_id,key,evidence) VALUES($1,$2,$3,'test',$4,'writer-account','{"settings":{}}')`, []any{id.UUID(), psp, string(rail), account}},
	} {
		_, err := source.Qx(ctx).Exec(ctx, seed.sql, seed.args...)
		require.NoError(t, err)
	}
	services := newPurchaseArchiveServices(source, clock)
	if phase == "pending" || phase == "activated" {
		before, until := now, now.Add(72*time.Hour)
		for _, name := range []string{"archive_access", "archive_download"} {
			_, err := services.entitlements.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: customer.String(), Entitlement: name, NotBefore: &before, EndAt: &until, SourceType: models.EntitlementSourceAdmin, SourceID: uuid.New()})
			require.NoError(t, err)
		}
	}
	transaction := "archive-purchase-" + uuid.NewString()
	end, providerSubscription := now.Add(48*time.Hour), "archive-sub-"+uuid.NewString()
	methodID := uuid.New()
	method := &models.PaymentMethod{ID: methodID, CustomerID: customer, PspID: psp, Rail: rail, RailCustomerRef: "archive-vault", RailMethodRef: "archive-card", InitialTransactionID: "archive-initial", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(source).Create(ctx, method))
	_, err = source.Qx(ctx).Exec(ctx, `INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,plan_id) VALUES($1,$2,$3,'writer-plan')`, id.UUID(), price, psp)
	require.NoError(t, err)
	var gatewayCalls atomic.Int64
	var cutoverArmed, sourceCanceled atomic.Bool
	var sourceDeletes atomic.Int64
	var readMu sync.Mutex
	var acceptedForm url.Values
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if phase == "cutover" && cutoverArmed.Load() {
			plan := nmi.V5Plan{Object: "plan", ID: "writer-plan", PlanAmount: "2.50", DayFrequency: "2", PlanPayments: "0"}
			if r.Method == http.MethodGet && r.URL.Path == "/plans/writer-plan" {
				require.NoError(t, json.NewEncoder(w).Encode(plan))
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/subscriptions/"+providerSubscription {
				status := "active"
				if sourceCanceled.Load() {
					status = "inactive"
				}
				require.NoError(t, json.NewEncoder(w).Encode(nmi.V5Subscription{Object: "subscription", ID: providerSubscription, CustomerVaultID: "archive-vault", DelayedCondition: status, PausedSubscription: false, Amount: "2.50", NextBillingDate: end.UTC().Format(time.RFC3339), Plan: &plan}))
				return
			}
			if r.Method == http.MethodDelete && r.URL.Path == "/subscriptions/"+providerSubscription {
				sourceCanceled.Store(true)
				sourceDeletes.Add(1)
				fmt.Fprint(w, `{}`)
				return
			}
		}
		if recurring && r.Method == http.MethodGet && r.URL.Path == "/customers/archive-vault" {
			fmt.Fprint(w, `{"object":"customer","id":"archive-vault","billing":[{"id":"archive-card","priority":1}]}`)
			return
		}
		if r.Method == http.MethodGet || r.Form.Get("order_id") != "" || r.Form.Get("report_type") == "recurring" {
			readMu.Lock()
			defer readMu.Unlock()
			if acceptedForm == nil {
				http.NotFound(w, r)
				return
			}
			if recurring && r.Method == http.MethodGet && r.URL.Path == "/subscriptions/"+providerSubscription {
				start, err := time.Parse("20060102", acceptedForm.Get("start_date"))
				require.NoError(t, err)
				_ = json.NewEncoder(w).Encode(nmi.V5Subscription{Object: "subscription", ID: providerSubscription, CustomerVaultID: "archive-vault", DelayedCondition: "active", PausedSubscription: false, NextBillingDate: start.Format("2006-01-02"), Plan: &nmi.V5Plan{ID: "writer-plan", PlanAmount: "2.50", DayFrequency: "2", PlanPayments: "0"}})
			} else if recurring && r.Form.Get("report_type") == "recurring" {
				start, err := time.Parse("20060102", acceptedForm.Get("start_date"))
				require.NoError(t, err)
				fmt.Fprintf(w, `<nm_response><subscription id="%s"><subscription_id>%s</subscription_id><plan><plan_id>writer-plan</plan_id></plan><orderid>%s</orderid><ponumber>%s</ponumber><next_charge_date>%s</next_charge_date></subscription></nm_response>`, providerSubscription, providerSubscription, acceptedForm.Get("orderid"), acceptedForm.Get("ponumber"), start.Format("2006-01-02"))
			} else if r.Method == http.MethodGet {
				require.Equal(t, "/payments/"+transaction, r.URL.Path)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": transaction, "response": "1", "amount": acceptedForm.Get("amount"), "currency": acceptedForm.Get("currency"), "customer_vault_id": acceptedForm.Get("customer_vault_id"), "actions": []map[string]any{{"id": transaction, "type": "sale", "success": true, "amount": acceptedForm.Get("amount")}}})
			} else if r.Form.Get("order_id") == acceptedForm.Get("orderid") {
				fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, transaction, acceptedForm.Get("orderid"))
			} else {
				fmt.Fprint(w, `<nm_response></nm_response>`)
			}
			return
		}
		operationMatches := r.Form.Get("type") == "sale" && r.Form.Get("recurring") == ""
		if recurring {
			operationMatches = r.Form.Get("recurring") == "add_subscription" && r.Form.Get("plan_id") == "writer-plan"
		}
		if r.Method != http.MethodPost || !operationMatches {
			t.Errorf("unexpected NMI operation: method=%s type=%s recurring=%s", r.Method, r.Form.Get("type"), r.Form.Get("recurring"))
			http.Error(w, "unexpected provider call", http.StatusBadRequest)
			return
		}
		expectedAmount := "2.50"
		if phase == "pending" || phase == "activated" {
			expectedAmount = ""
		}
		if r.Form.Get("amount") != expectedAmount || r.Form.Get("customer_vault_id") != "archive-vault" {
			t.Error("wrong frozen charge amount or vault")
			http.Error(w, "wrong charge", http.StatusBadRequest)
			return
		}
		gatewayCalls.Add(1)
		if phase == "refused" {
			fmt.Fprint(w, "response=2&responsetext=Declined&response_code=200")
			return
		}
		readMu.Lock()
		acceptedForm = r.Form
		readMu.Unlock()
		replyTransaction := transaction
		if phase == "pending" || phase == "activated" {
			replyTransaction = ""
		}
		fmt.Fprintf(w, "response=1&responsetext=SUCCESS&transactionid=%s&subscription_id=%s&response_code=100", replyTransaction, providerSubscription)
	}))
	t.Cleanup(gateway.Close)
	client, err := nmi.NewAccountClient(id.UUID(), psp, "writer-account", &config.NMIProviderSettings{SecurityKey: "archive-fixture-key", WebhookSecret: "archive-fixture-webhook"}, true)
	require.NoError(t, err)
	client.DirectPostURL, client.QueryURL, client.V5BaseURL = gateway.URL, gateway.URL, gateway.URL
	provider := purchaseArchiveProvider{scope: merchants.PSPScope{ID: psp, Rail: "nmi", Key: "writer-account", Environment: "test", AccountID: account}}
	checkoutService := newPurchaseArchiveCheckout(source, services, clock, provider, client)
	sessionService := newPurchaseArchiveSession(source, checkoutService, clock)
	clientKey := "archive-checkout-" + uuid.NewString()
	sessionRequest := func() *checkout.CheckoutSessionCreateRequest {
		return &checkout.CheckoutSessionCreateRequest{PriceID: openrails.PriceID(price).String(), Payment: checkout.CheckoutSessionPaymentRequest{Rail: "writer-account", PaymentMethodID: openrails.PaymentMethodID(methodID).String()}, IdempotencyKey: clientKey}
	}
	user := &checkout.UserIdentity{ID: customer.String()}
	if phase != "" && phase != "cutover" {
		result, checkoutErr := checkoutService.Checkout(ctx, &checkout.CheckoutRequest{PriceID: openrails.PriceID(price).String(), Rail: "writer-account", PaymentMethodID: openrails.PaymentMethodID(methodID).String(), IdempotencyKey: clientKey}, user)
		if phase == "refused" {
			require.Error(t, checkoutErr)
		} else {
			require.NoError(t, checkoutErr)
			require.NotNil(t, result)
		}
		operation, err := intents.NewStore(source).GetByIdempotencyKey(ctx, checkout.InitialMembershipIdempotencyKey(clientKey))
		require.NoError(t, err)
		payload, err := subscriptions.DecodeInitialMembershipPayload(operation)
		require.NoError(t, err)
		if phase == "pending" || phase == "activated" {
			require.True(t, payload.Terms.Pending)
		} else {
			require.Equal(t, intents.StatusFailedTerminal, operation.Status)
		}
		var clean bytes.Buffer
		err = Export(ctx, source, id, &clean)
		require.NoError(t, err, "baseline archive before corruption must be supported")
		if phase == "refused" {
			for _, mutation := range []struct{ column, value string }{{"list_amount", "2500001"}, {"attempt_kind", "renewal"}, {"failure_code", "201"}, {"failure_reason", "insufficient_funds"}, {"purchased_at", now.Add(time.Hour).Format(time.RFC3339Nano)}, {"created_at", now.Add(time.Hour).Format(time.RFC3339Nano)}} {
				t.Run("failed_attempt_"+mutation.column, func(t *testing.T) {
					_, err := Restore(ctx, target, id, bytes.NewReader(alteredArchive(t, clean.Bytes(), "payments", mutation.column, &mutation.value)))
					require.Error(t, err, "failed payment must match sealed refusal and accepted attempt")
					assertEmptyBook(t, target, id)
				})
			}
		}

		if phase == "activated" {
			clock.Advance(payload.Terms.PeriodStart.Add(time.Hour).Sub(clock.Now()))
			observed := payload.Terms
			observed.Pending = false
			observed.Amount = observed.RecurringAmount
			observed.PaymentID = uuid.New()
			transaction := "observed-first-" + uuid.NewString()
			purchased := clock.Now()
			err := source.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, _, err := services.lifecycle.CreateMembershipTx(ctx, source.NewWithPgxTx(tx), &subscriptions.CreateMembershipParams{Prepared: &observed, UserID: customer.String(), PriceID: price, Rail: models.RailNMI, RailSubscriptionID: &providerSubscription, TransactionID: transaction, PurchasedAt: &purchased, PaymentMetadata: map[string]any{"order_id": intents.NMIEnrollmentOrder(operation), "provider_transaction_id": transaction}})
				return err
			})
			require.NoError(t, err)
			_, err = source.Qx(ctx).Exec(ctx, `UPDATE openrails.host_outbox SET delivered_at=$2 WHERE merchant_id=$1`, id.UUID(), clock.Now())
			require.NoError(t, err)
			var activated bytes.Buffer
			require.NoError(t, Export(ctx, source, id, &activated), "first observed payment preserves original no-charge enrollment")
			for _, mutation := range []struct{ column, value string }{{"amount", "2500001"}, {"list_amount", "2500001"}, {"status", "failed"}} {
				_, err := Restore(ctx, target, id, bytes.NewReader(alteredArchive(t, activated.Bytes(), "payments", mutation.column, &mutation.value)))
				require.Error(t, err, "first observed payment cannot contradict frozen terms")
				assertEmptyBook(t, target, id)
			}
			_, err = Restore(ctx, target, id, bytes.NewReader(activated.Bytes()))
			require.NoError(t, err)
			return
		}
		probeAt := payload.Terms.PeriodEnd.Add(-time.Hour)
		entitledBefore, err := services.entitlements.IsCustomerEntitled(ctx, customer, "archive_access", probeAt)
		require.NoError(t, err)
		require.False(t, entitledBefore, "baseline has no paid access in the probed window")
		grantID := uuid.New()
		_, err = source.Qx(ctx).Exec(ctx, `INSERT INTO openrails.grants(merchant_id,id,customer_id,product_id,kind,source_type,source_id,event,spec_snapshot,starts_at,ends_at) VALUES($1,$2,$3,$4,'entitlement','subscription',$5,'grant','{"entitlements":["archive_access"]}',$6,$7)`, id.UUID(), grantID, customer, product, payload.Terms.SubscriptionID.String(), payload.Terms.PeriodStart, payload.Terms.PeriodEnd)
		require.NoError(t, err)
		_, err = source.Qx(ctx).Exec(ctx, `INSERT INTO openrails.entitlements(merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type,grant_id) VALUES($1,$2,'archive_access',$3,$4,$5,'subscription',$6)`, id.UUID(), customer, payload.Terms.PeriodStart, payload.Terms.PeriodEnd, payload.Terms.SubscriptionID, grantID)
		require.NoError(t, err)
		var rejected bytes.Buffer
		err = Export(ctx, source, id, &rejected)
		require.Error(t, err, "export must refuse an unpaid subscription source grant")
		corrupted := encodeUncheckedInitialArchive(t, ctx, source, id)
		_, err = archivewire.CopyVerified(io.Discard, bytes.NewReader(corrupted))
		require.NoError(t, err, "corruption has a valid wire footer and row count")
		_, err = Restore(ctx, target, id, bytes.NewReader(corrupted))
		require.Error(t, err, "restore must reject semantic unpaid access, not merely the digest")
		assertEmptyBook(t, target, id)
		t.Logf("FIX CONFIRMED %s: export and coherent-wire restore reject unpaid access; destination empty", phase)
		return
	}
	firstRequest := sessionRequest()
	first, err := sessionService.CreateSession(ctx, firstRequest, user)
	require.NoError(t, err)
	require.Equal(t, "succeeded", first.Status)
	require.Equal(t, transaction, first.Payment.TransactionID)
	require.EqualValues(t, 1, gatewayCalls.Load())
	sessionID := first.ID.UUID()
	first, err = sessionService.GetSession(ctx, sessionID, user)
	require.NoError(t, err)
	payment, err := services.payments.GetByPSPTransactionID(ctx, rail, transaction)
	require.NoError(t, err)
	paymentID := payment.ID
	var subscriptionID uuid.UUID
	if recurring {
		require.NotNil(t, first.SubscriptionID)
		subscriptionID = first.SubscriptionID.UUID()
	}
	require.NotEqual(t, firstRequest.IdempotencyKey, sessionID.String(), "caller replay key is distinct from persisted session identity")
	operationKey := checkout.NMISaleIdempotencyKey("checkout_native_session:" + sessionID.String())
	if recurring {
		operationKey = checkout.InitialMembershipIdempotencyKey("checkout_native_session:" + sessionID.String())
	}
	operation, err := intents.NewStore(source).GetByIdempotencyKey(ctx, operationKey)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, operation.Status)
	require.NotEmpty(t, operation.ResultEvidence, "ordinary durable checkout must retain replay receipt")
	if recurring {
		require.NoError(t, intents.ValidateInitialMembershipTerminal(operation))
		require.NotEmpty(t, operation.Payload, "accepted enrollment terms and both receipts remain in custody")
	} else {
		require.NoError(t, intents.ValidateNMISaleTerminal(operation))
		require.NotEmpty(t, operation.Payload, "accepted sale terms and custody survive terminal replay")
	}
	before := readPurchaseArchiveState(t, ctx, source, services, id, customer, product, rail, transaction, now)
	require.Equal(t, paymentID, before.payment.ID)
	require.Equal(t, int64(2500000), before.payment.Amount)
	require.Equal(t, "USD", before.payment.Currency)
	require.Equal(t, customer, before.payment.CustomerID)
	require.Equal(t, price, before.payment.PriceID)
	require.Equal(t, &psp, before.payment.PspID)
	require.Len(t, before.entitlements, 2)
	require.NotEqual(t, "[]", before.grants, "purchase writers must actually create benefits")
	for _, entitlement := range before.entitlements {
		require.True(t, now.Equal(entitlement.StartAt))
		if recurring {
			require.Nil(t, entitlement.EndAt)
		} else {
			require.NotNil(t, entitlement.EndAt)
			require.True(t, end.Equal(*entitlement.EndAt))
		}
	}
	if recurring {
		require.Equal(t, &subscriptionID, before.payment.SubscriptionID)
		require.Equal(t, models.StatusActive, before.subscription.Status)
		require.Equal(t, providerSubscription, before.subscription.RailSubscriptionID)
		require.True(t, now.Equal(*before.subscription.CurrentPeriodStartsAt))
		require.True(t, end.Equal(*before.subscription.CurrentPeriodEndsAt))
	} else {
		require.Nil(t, before.payment.SubscriptionID)
		require.True(t, before.ownsProduct)
	}
	var otherPaymentID uuid.UUID
	if !recurring {
		otherPaymentID = uuid.New()
		require.NoError(t, services.payments.Create(ctx, &models.Payment{ID: otherPaymentID, CustomerID: uuid.New(), PriceID: price, Rail: rail, TransactionID: "unrelated-decline-" + otherPaymentID.String(), Amount: 2500000, ListAmount: 2500000, Currency: "USD", Status: payments.PaymentStatusFailedValue, MoneyMovement: models.MoneyMovementNone, PurchasedAt: now}))
	}
	// Complete the host's delivery obligation through the same queries used by
	// ListHostEvents/AcknowledgeHostEvent before the controlled archive cutover.
	events, err := source.Gen(ctx).ListHostEvents(ctx, gen.ListHostEventsParams{MerchantID: id.UUID(), RowLimit: 100})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, &paymentID, events[0].PaymentID)
	acknowledged, err := source.Gen(ctx).AcknowledgeHostEvent(ctx, gen.AcknowledgeHostEventParams{MerchantID: id.UUID(), ID: events[0].ID, Now: now})
	require.NoError(t, err)
	require.EqualValues(t, 1, acknowledged)
	sourceReplay, err := sessionService.CreateSession(ctx, sessionRequest(), user)
	require.NoError(t, err)
	require.Equal(t, first, sourceReplay, "source and destination use the same session replay path")
	require.EqualValues(t, 1, gatewayCalls.Load())
	if phase == "cutover" {
		cutoverArmed.Store(true)
		qualifiedInitialArchiveCutover(t, ctx, source, id, customer, price, subscriptionID, psp, client, clock, end)
		require.True(t, sourceCanceled.Load())
		require.EqualValues(t, 1, sourceDeletes.Load())
		before = readPurchaseArchiveState(t, ctx, source, services, id, customer, product, rail, transaction, now)
		require.Equal(t, &psp, before.payment.PspID, "original paid receipt remains on its original account")
		require.NotEqual(t, psp, before.subscription.PspID)
		require.EqualValues(t, 1, gatewayCalls.Load(), "provider cutover must not add a sale")
	}
	release()

	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &artifact), "ordinary purchase/lifecycle writer state must be portable")
	for _, tc := range []struct{ name, table, column, value string }{
		{"unknown payment metadata", "payments", "metadata", `{"unknown":"retained"}`},
		{"nested payment metadata", "payments", "metadata", `{"order_id":{"raw":"body"}}`},
		{"credential payment metadata", "payments", "metadata", `{"provider_transaction_id":"sk_live_must_refuse"}`},
		{"incomplete NMI payload", "rail_intents", "payload", `{"customer_vault_id":"archive-vault"}`},
		{"credential NMI payload", "rail_intents", "payload", `{"security_key":"must_refuse"}`},
		{"unknown NMI receipt", "rail_intents", "result_evidence", `{"raw_body":{"secret":"must_refuse"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, artifact.Bytes(), tc.table, tc.column, &tc.value)))
			require.Error(t, err)
			assertEmptyBook(t, target, id)
		})
	}
	if !recurring {
		var payload, evidence map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(operation.Payload, &payload))
		require.NoError(t, json.Unmarshal(operation.ResultEvidence, &evidence))
		payload["amount"] = json.RawMessage(`"2500001"`)
		changedMoney, err := json.Marshal(payload)
		require.NoError(t, err)
		evidence["payment_id"], err = json.Marshal(otherPaymentID.String())
		require.NoError(t, err)
		wrongPayment, err := json.Marshal(evidence)
		require.NoError(t, err)
		evidence["payment_id"], err = json.Marshal(paymentID.String())
		require.NoError(t, err)
		delete(evidence, "qualified_receipt")
		missingReceipt, err := json.Marshal(evidence)
		require.NoError(t, err)
		for _, tc := range []struct{ name, column, value string }{{"changed accepted money", "payload", string(changedMoney)}, {"missing qualified payment custody", "result_evidence", string(missingReceipt)}, {"another real payment", "result_evidence", string(wrongPayment)}} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, artifact.Bytes(), "rail_intents", tc.column, &tc.value)))
				require.Error(t, err)
				assertEmptyBook(t, target, id)
			})
		}
		t.Run("changed original benefit window", func(t *testing.T) {
			changed := now.Add(time.Hour).UTC().Format("2006-01-02 15:04:05.999999-07")
			_, err := Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, artifact.Bytes(), "grants", "starts_at", &changed)))
			require.Error(t, err)
			assertEmptyBook(t, target, id)
		})
	}
	if recurring {
		t.Run("conflicting_refusal_custody", func(t *testing.T) {
			var evidence map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(operation.ResultEvidence, &evidence))
			var paid struct {
				Binding json.RawMessage `json:"binding"`
			}
			require.NoError(t, json.Unmarshal(evidence["qualified_receipt"], &paid))
			refusal, err := json.Marshal(map[string]any{"binding": paid.Binding, "kind": "provider_declined", "response_code": 200, "localization_id": ""})
			require.NoError(t, err)
			evidence["qualified_initial_refusal"] = refusal
			encoded, err := json.Marshal(evidence)
			require.NoError(t, err)
			changed := string(encoded)
			_, err = Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, artifact.Bytes(), "rail_intents", "result_evidence", &changed)))
			require.Error(t, err, "successful membership cannot carry simultaneous sealed refusal")
			assertEmptyBook(t, target, id)
		})
		var evidence map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(operation.ResultEvidence, &evidence))
		delete(evidence, "qualified_enrollment")
		changed, err := json.Marshal(evidence)
		require.NoError(t, err)
		missing := string(changed)
		_, err = Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, artifact.Bytes(), "rail_intents", "result_evidence", &missing)))
		require.Error(t, err)
		assertEmptyBook(t, target, id)
		start := now.Add(time.Hour).UTC().Format("2006-01-02 15:04:05.999999-07")
		_, err = Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, artifact.Bytes(), "grants", "starts_at", &start)))
		require.Error(t, err)
		assertEmptyBook(t, target, id)
	}
	_, err = Restore(t.Context(), target, id, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	var restored bytes.Buffer
	require.NoError(t, Export(t.Context(), target, id, &restored))
	require.Equal(t, artifact.String(), restored.String(), "all retained billing references survive")

	targetCtx, releaseTarget, err := target.WithMerchantConn(merchant.WithID(t.Context(), id))
	require.NoError(t, err)
	defer releaseTarget()
	targetCtx = db.WithPSPID(targetCtx, psp)
	restoredServices := newPurchaseArchiveServices(target, clock)
	after := readPurchaseArchiveState(t, targetCtx, target, restoredServices, id, customer, product, rail, transaction, now)
	require.Equal(t, before, after)
	clock.Advance(time.Hour)
	restoredCheckout := newPurchaseArchiveCheckout(target, restoredServices, clock, provider, client)
	restoredSession := newPurchaseArchiveSession(target, restoredCheckout, clock)
	readBack, err := restoredSession.GetSession(targetCtx, sessionID, user)
	require.NoError(t, err)
	require.Equal(t, first, readBack, "ordinary restored session read returns original result")
	replay, err := restoredSession.CreateSession(targetCtx, sessionRequest(), user)
	require.NoError(t, err)
	require.Equal(t, first, replay, "fresh process/cache replays the durable session")
	confirmed, err := restoredSession.ConfirmSession(targetCtx, sessionID, &checkout.CheckoutSessionConfirmRequest{}, user)
	require.NoError(t, err)
	require.Equal(t, first, confirmed)
	require.EqualValues(t, 1, gatewayCalls.Load(), "restored checkout cannot charge or enroll again")
	replayOperation, err := (&intents.Runner{Store: intents.NewStore(target)}).ExecuteByID(targetCtx, operation.ID)
	require.NoError(t, err)
	require.Equal(t, operation.ID, replayOperation.ID)
	require.Equal(t, intents.StatusSucceeded, replayOperation.Status)
	require.JSONEq(t, string(operation.ResultEvidence), string(replayOperation.ResultEvidence))
	again := readPurchaseArchiveState(t, targetCtx, target, restoredServices, id, customer, product, rail, transaction, now)
	require.Equal(t, after, again, "replay neither duplicates benefits nor extends paid time")
	var paymentCount, subscriptionCount, undelivered int
	require.NoError(t, target.Qx(targetCtx).QueryRow(targetCtx, `SELECT count(*) FROM openrails.payments WHERE merchant_id=$1`, id.UUID()).Scan(&paymentCount))
	if recurring {
		require.Equal(t, 1, paymentCount)
	} else {
		require.Equal(t, 2, paymentCount, "one purchase and the unrelated failed attempt")
	}
	require.NoError(t, target.Qx(targetCtx).QueryRow(targetCtx, `SELECT count(*) FROM openrails.subscriptions WHERE merchant_id=$1`, id.UUID()).Scan(&subscriptionCount))
	if recurring {
		require.Equal(t, 1, subscriptionCount)
	} else {
		require.Zero(t, subscriptionCount)
	}
	require.NoError(t, target.Qx(targetCtx).QueryRow(targetCtx, `SELECT count(*) FROM openrails.host_outbox WHERE merchant_id=$1 AND delivered_at IS NULL`, id.UUID()).Scan(&undelivered))
	require.Zero(t, undelivered, "replay must not emit another payment-settled event")
}

type purchaseArchiveState struct {
	payment      *models.Payment
	entitlements []models.Entitlement
	grants       string
	ownsProduct  bool
	subscription *models.Subscription
}

func readPurchaseArchiveState(t *testing.T, ctx context.Context, d *db.DB, services purchaseArchiveServices, id merchant.ID, customer, product uuid.UUID, rail models.Rail, transaction string, at time.Time) purchaseArchiveState {
	t.Helper()
	payment, err := services.payments.GetByPSPTransactionID(ctx, rail, transaction)
	require.NoError(t, err)
	ents, err := services.entitlements.ListActiveRecords(ctx, customer.String(), at)
	require.NoError(t, err)
	sort.Slice(ents, func(i, j int) bool { return ents[i].Entitlement < ents[j].Entitlement })
	for _, ent := range ents {
		active, err := services.entitlements.IsCustomerEntitled(ctx, customer, ent.Entitlement, at)
		require.NoError(t, err)
		require.True(t, active)
	}
	owned, err := services.access.HasProductAccess(ctx, customer.String(), product)
	require.NoError(t, err)
	var grantRows string
	require.NoError(t, d.Qx(ctx).QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(g) ORDER BY g.id),'[]'::jsonb)::text FROM openrails.grants g WHERE merchant_id=$1`, id.UUID()).Scan(&grantRows))
	var sub *models.Subscription
	if payment.SubscriptionID != nil {
		sub, err = services.subscriptions.GetByID(ctx, *payment.SubscriptionID)
		require.NoError(t, err)
	}
	return purchaseArchiveState{payment, ents, grantRows, owned, sub}
}

// The cache is deliberately empty on both deployments: the persisted session
// and intent must answer the restored request without process-local state.
type purchaseArchiveEmptyCache struct{}

func (purchaseArchiveEmptyCache) Begin(context.Context, string, string) (*checkout.IdempotencyRecord, bool, error) {
	return nil, false, nil
}
func (purchaseArchiveEmptyCache) Fail(context.Context, string, string, error) error { return nil }
func (purchaseArchiveEmptyCache) Complete(context.Context, string, string, json.RawMessage) error {
	return nil
}

type purchaseArchiveProvider struct{ scope merchants.PSPScope }

func (p purchaseArchiveProvider) ActivePSPSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, fmt.Errorf("unexpected secret read: loopback client is explicit")
}
func (p purchaseArchiveProvider) ActivePSPScope(context.Context, merchant.ID, string, string) (merchants.PSPScope, bool, error) {
	return p.scope, true, nil
}
func (p purchaseArchiveProvider) PSPScopeByKey(_ context.Context, _ merchant.ID, key, _ string) (merchants.PSPScope, bool, error) {
	return p.scope, key == p.scope.Key, nil
}

func newPurchaseArchiveCheckout(d *db.DB, s purchaseArchiveServices, clock clockwork.Clock, provider purchaseArchiveProvider, client *nmi.NMIClient) *checkout.CheckoutService {
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
	railMethods := &paymentmethods.RailPaymentMethodService{DB: d}
	c := checkout.NewCheckoutService(s.subscriptions, catalog.NewProductService(d), catalog.NewPriceService(d), s.payments, s.entitlements, paymentmethods.NewPaymentMethodService(d), railMethods, purchaseArchiveEmptyCache{}, nil, cfg, railresolve.FixedSet{"nmi": {Rail: models.RailNMI, AccountID: provider.scope.AccountID, NMI: &config.NMIRailConfig{SecurityKey: "archive-fixture-key"}}}, clock)
	c.PurchaseService.SetProductAccessService(s.access)
	c.ProviderSecrets = provider
	c.ResolveNMIClientOverride = func(context.Context, string) (*nmi.NMIClient, error) { return client, nil }
	c.SetSubscriptionLifecycleService(s.lifecycle)
	runner := &intents.Runner{Store: intents.NewStore(d), Registry: intents.NewRegistry(checkout.NewNMISaleIntentHandler(c.NMISaleService), checkout.NewInitialMembershipIntentHandler(c)), Config: cfg}
	c.Intents, c.NMISaleService.Intents = runner, runner
	return c
}

func newPurchaseArchiveSession(d *db.DB, c *checkout.CheckoutService, clock clockwork.Clock) *checkout.CheckoutSessionService {
	return checkout.NewCheckoutSessionService(d, c.PriceService, c.ProductService, c.PaymentMethodService, nil, c, nil, nil, nil, nil, c.Config, c.Rails, clock)
}

func encodeUncheckedInitialArchive(t *testing.T, ctx context.Context, d *db.DB, id merchant.ID) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := archivewire.NewWriter(&out, id.String())
	require.NoError(t, err)
	for _, profile := range contract.Profiles {
		require.NoError(t, w.Table(profile.Name))
		rows, err := d.Qx(ctx).Query(ctx, exportQuery(profile), id.UUID())
		require.NoError(t, err)
		for rows.Next() {
			values := make([]*string, len(profile.Columns))
			dest := make([]any, len(values))
			for i := range values {
				dest[i] = &values[i]
			}
			require.NoError(t, rows.Scan(dest...))
			require.NoError(t, contract.ValidateValues(profile, values), profile.Name)
			require.NoError(t, w.Row(values))
		}
		require.NoError(t, rows.Err())
		rows.Close()
	}
	require.NoError(t, w.Close())
	return out.Bytes()
}
func TestInitialMembershipRefusalArchiveRejectsOrphanGrant(t *testing.T) {
	testPurchaseWorkflowArchive(t, true, "refused")
}
func TestInitialMembershipPendingArchiveRejectsStartGrant(t *testing.T) {
	testPurchaseWorkflowArchive(t, true, "pending")
}

func TestInitialMembershipObservedPaymentArchive(t *testing.T) {
	testPurchaseWorkflowArchive(t, true, "activated")
}
