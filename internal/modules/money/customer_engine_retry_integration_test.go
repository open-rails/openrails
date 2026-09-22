//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/testfixture"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

type customerEngineFixture struct {
	nmiReceiptEnv
	sub       uuid.UUID
	clock     *clockwork.FakeClock
	principal billingauth.DelegatedPrincipal
}

func newCustomerEngineFixture(t *testing.T) customerEngineFixture {
	t.Helper()
	e := newNMIReceiptEnv(t)
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)
	e.svc.SetClock(clock)
	mid := dbtest.TestMerchantID.UUID()
	product, price, sub := uuid.New(), uuid.New(), uuid.New()
	_, err := e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET rail_method_ref='engine-billing',stored_credential_recurring_ref='' WHERE id=$1`, e.method)
	require.NoError(t, err)
	method := e.methodRow(t)
	_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$1::uuid::text,'Engine retry')`, product, mid)
	require.NoError(t, err)
	_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,9990000,'USD',true,720)`, price, mid, product)
	require.NoError(t, err)
	testfixture.EngineMembership(t, e.ctx, e.db, subscriptions.InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyEngine, SubscriptionID: sub, PaymentID: uuid.New(), CustomerID: e.payer.UUID(), PSPID: method.PspID, ProductID: product, PriceID: price, PaymentMethodID: e.method, ProductName: "Engine retry", Amount: 9990000, RecurringAmount: 9990000, Currency: "USD", AcceptedAt: now.Add(-720 * time.Hour), PeriodStart: now.Add(-720 * time.Hour), PeriodEnd: now})
	f := customerEngineFixture{nmiReceiptEnv: e, sub: sub, clock: clock, principal: billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: e.payer.UUID().String()}}
	f.declineDue(t)
	return f
}

func (f customerEngineFixture) declineDue(t *testing.T) {
	t.Helper()
	op, err := f.svc.AdmitDueSubscriptionCollection(f.ctx, f.sub, f.clock.Now())
	require.NoError(t, err)
	op, claimed, err := intents.NewStore(f.db).ClaimByID(f.ctx, op.ID, f.clock.Now(), f.clock.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	f.gateway.mu.Lock()
	f.gateway.saleResponse = "response=2&response_code=200"
	f.gateway.mu.Unlock()
	outcome := money.NewSubscriptionCollectionHandler(f.db, f.plane, f.plane.Config, f.clock).Execute(f.ctx, op)
	require.Equal(t, intents.OutcomeTerminal, outcome.Class, outcome.Reason)
}

func (f customerEngineFixture) retry(t *testing.T, key string, method *uuid.UUID) (gen.OpenrailsRailIntent, bool, error) {
	t.Helper()
	return f.svc.AdmitCustomerSubscriptionCollection(f.ctx, f.sub, f.payer.UUID(), key, method, f.principal)
}

func TestCustomerEngineRetryEligibilityReadback(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		unsupported bool
	}{
		{"nil retry schedule", `UPDATE billing.subscriptions SET next_retry_at=NULL WHERE id=$1`, false},
		{"changed paid boundary", `UPDATE billing.subscriptions SET current_period_starts_at=current_period_starts_at-interval '1 second' WHERE id=$1`, false},
		{"cancellation marker", `UPDATE billing.subscriptions SET cancelled_at=now() WHERE id=$1`, false},
		{"deletion marker", `UPDATE billing.subscriptions SET deletion_scheduled_at=now() WHERE id=$1`, false},
		{"archived PSP", `UPDATE billing.psps SET archived=true WHERE id=(SELECT psp_id FROM billing.subscriptions WHERE id=$1)`, true},
		{"parked method", `UPDATE billing.payment_methods SET park_reason='unavailable' WHERE id=(SELECT payment_method_id FROM billing.subscriptions WHERE id=$1)`, true},
		{"missing paid agreement", `UPDATE billing.rail_intents SET status='unknown_needs_verify' WHERE intent_type='initial_membership' AND payload->'terms'->>'subscription_id'=$1::uuid::text`, false},
		{"corrupt paid receipt", `UPDATE billing.rail_intents SET result_evidence='{}' WHERE intent_type='initial_membership' AND payload->'terms'->>'subscription_id'=$1::uuid::text`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCustomerEngineFixture(t)
			_, err := f.pool.Exec(f.ctx, tc.query, f.sub)
			require.NoError(t, err)
			_, _, err = f.retry(t, "refused-"+uuid.NewString(), nil)
			want := intents.ErrRebillNotRetryable
			if tc.unsupported {
				want = intents.ErrRebillUnsupported
			}
			require.ErrorIs(t, err, want)
			svc, err := service.New(&app.Runtime{DB: f.db, Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, Clock: f.clock, MoneyService: f.svc, EntitlementService: entitlements.NewEntitlementService(f.db, f.clock)})
			require.NoError(t, err)
			recovery, err := svc.SubscriptionRecovery(f.ctx, f.payer, f.sub)
			require.NoError(t, err)
			require.False(t, recovery.Retryable)
			f.gateway.mu.Lock()
			defer f.gateway.mu.Unlock()
			require.Equal(t, 1, f.gateway.sends, "a refused customer retry must not submit another provider write")
		})
	}
}

func TestCustomerEngineRetryExactReplayAndExplicitInitiator(t *testing.T) {
	f := newCustomerEngineFixture(t)
	op, replayed, err := f.retry(t, "accepted", &f.method)
	require.NoError(t, err)
	require.False(t, replayed)
	require.True(t, charge.CustomerPaymentKeyValid(subscriptions.TypeManualRebill, f.payer.UUID(), op.IdempotencyKey))
	_, _, err = f.retry(t, "accepted", nil)
	require.ErrorIs(t, err, intents.ErrRebillKeyConflict)
	bad := op
	p, err := subscriptions.DecodeSubscriptionCollectionPayload(op)
	require.NoError(t, err)
	p.Initiator = ""
	bad.Payload, err = json.Marshal(p)
	require.NoError(t, err)
	_, err = subscriptions.DecodeSubscriptionCollectionPayload(bad)
	require.Error(t, err)
	// Current lifecycle refusals cannot change the result of an accepted key.
	_, err = f.pool.Exec(f.ctx, `UPDATE billing.subscriptions SET next_retry_at=NULL,deletion_scheduled_at=now(),cancelled_at=now() WHERE id=$1`, f.sub)
	require.NoError(t, err)
	f.svc.EngineAdmissionHold = true
	same, replayed, err := f.retry(t, "accepted", &f.method)
	require.NoError(t, err)
	require.True(t, replayed)
	require.Equal(t, op.ID, same.ID)
	// The native endpoint shares the namespace and reports a typed conflict.
	h := intents.NewManualRebillHandler(f.db, f.plane.Config, f.plane, f.clock)
	_, _, err = h.EnqueueCustomer(f.ctx, f.sub, f.payer.UUID(), "accepted", &f.method)
	require.ErrorIs(t, err, intents.ErrRebillKeyConflict)
}

func TestCustomerEngineRetryWorkerRaceKeepsOneObligation(t *testing.T) {
	f := newCustomerEngineFixture(t)
	_, err := f.pool.Exec(f.ctx, `UPDATE billing.subscriptions SET next_retry_at=$2 WHERE id=$1`, f.sub, f.clock.Now())
	require.NoError(t, err)
	var wg sync.WaitGroup
	rows := make(chan gen.OpenrailsRailIntent, 2)
	errs := make(chan error, 2)
	wg.Go(func() {
		row, err := f.svc.AdmitDueSubscriptionCollection(f.ctx, f.sub, f.clock.Now())
		rows <- row
		errs <- err
	})
	wg.Go(func() { row, _, err := f.retry(t, "race", nil); rows <- row; errs <- err })
	wg.Wait()
	close(rows)
	close(errs)
	for err := range errs {
		require.True(t, err == nil || errors.Is(err, intents.ErrRebillInProgress), "unexpected race refusal: %v", err)
	}
	var id uuid.UUID
	for row := range rows {
		if row.ID == uuid.Nil {
			continue
		}
		if id == uuid.Nil {
			id = row.ID
		} else {
			require.Equal(t, id, row.ID)
		}
	}
	require.NotEqual(t, uuid.Nil, id)
	var count int
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='subscription_collection' AND status='pending'`, f.sub).Scan(&count))
	require.Equal(t, 1, count)
}

func TestCustomerEngineRetryRequiresVerifiedPrincipal(t *testing.T) {
	f := newCustomerEngineFixture(t)
	for _, change := range []func(*billingauth.DelegatedPrincipal){
		func(p *billingauth.DelegatedPrincipal) { p.CredentialClass = billingauth.CredentialClassAutomation },
		func(p *billingauth.DelegatedPrincipal) { p.Invoker = "tool" },
		func(p *billingauth.DelegatedPrincipal) { p.SubjectID = uuid.NewString() },
		func(p *billingauth.DelegatedPrincipal) { p.MerchantID = uuid.NewString() },
		func(p *billingauth.DelegatedPrincipal) { *p = billingauth.DelegatedPrincipal{} },
	} {
		p := f.principal
		change(&p)
		_, _, err := f.svc.AdmitCustomerSubscriptionCollection(context.WithoutCancel(f.ctx), f.sub, f.payer.UUID(), "authority", nil, p)
		require.ErrorIs(t, err, money.ErrCustomerSessionRequired)
	}
}

func TestCustomerEngineRetrySharesNativeEndpointKeyNamespace(t *testing.T) {
	f := newCustomerEngineFixture(t)
	product, price, legacy := uuid.New(), uuid.New(), uuid.New()
	method := f.methodRow(t)
	_, err := f.pool.Exec(f.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$1::uuid::text,'Imported native')`, product, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	_, err = f.pool.Exec(f.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,9990000,'USD',true,720)`, price, dbtest.TestMerchantID.UUID(), product)
	require.NoError(t, err)
	// Provider-owned imports keep their provider agreement; this is not an
	// alternate seed for an engine agreement or paid ledger evidence.
	_, err = f.pool.Exec(f.ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,collection_policy,rail_subscription_id,status,current_period_starts_at,current_period_ends_at,next_retry_at) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi','provider_dunning',$8,'past_due',$9,$10,$11)`, legacy, dbtest.TestMerchantID.UUID(), f.payer.UUID(), product, price, method.PspID, f.method, "provider-"+legacy.String(), f.clock.Now().Add(-720*time.Hour), f.clock.Now(), f.clock.Now().Add(time.Hour))
	require.NoError(t, err)
	native := intents.NewManualRebillHandler(f.db, f.plane.Config, f.plane, f.clock)
	accepted, _, err := native.EnqueueCustomer(f.ctx, legacy, f.payer.UUID(), "native-first", nil)
	require.NoError(t, err)
	_, _, err = f.retry(t, "native-first", nil)
	require.ErrorIs(t, err, intents.ErrRebillKeyConflict)
	unchanged, err := intents.NewStore(f.db).Get(f.ctx, accepted.ID)
	require.NoError(t, err)
	require.Equal(t, accepted.Payload, unchanged.Payload)
	engine, _, err := f.retry(t, "engine-first", nil)
	require.NoError(t, err)
	_, _, err = native.EnqueueCustomer(f.ctx, legacy, f.payer.UUID(), "engine-first", nil)
	require.ErrorIs(t, err, intents.ErrRebillKeyConflict)
	unchanged, err = intents.NewStore(f.db).Get(f.ctx, engine.ID)
	require.NoError(t, err)
	require.Equal(t, engine.Payload, unchanged.Payload)
}
