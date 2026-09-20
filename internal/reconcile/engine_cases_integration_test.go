//go:build integration

package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Each case has its own merchant and PSP. Only provider snapshots are fake;
// SQL, local-state loading, finding transitions and billing writes are real.
type reconcileCase struct {
	t        *testing.T
	db       *db.DB
	ctx      context.Context
	merchant merchant.ID
	psp      PSPBinding
	now      time.Time
	snap     *RemoteSnapshot
	engine   *Engine
}

func newReconcileCase(t *testing.T, provider Provider) *reconcileCase {
	t.Helper()
	database := startReconcilePostgres(t)
	mid := newReconcileMerchant(t, database)
	ctx := merchant.WithID(t.Context(), mid)
	f := &reconcileCase{t: t, db: database, ctx: ctx, merchant: mid, now: time.Now().UTC().Truncate(time.Second)}
	f.psp = seedTestPSPBindingFor(t, database, ctx, mid.UUID(), string(provider))
	f.snap = &RemoteSnapshot{Provider: provider, Capabilities: Capabilities{Subscriptions: true}}
	f.engine = newPGReconcileEngine(database, f.snap)
	f.engine.Fetchers = map[Provider]RailFetcher{provider: &fakeFetcher{provider: provider, snap: f.snap}}
	f.engine.Now = func() time.Time { return f.now }
	return f
}

func (f *reconcileCase) exec(sql string, args ...any) {
	f.t.Helper()
	require.NoError(f.t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		_, err := f.db.Qx(ctx).Exec(ctx, sql, args...)
		return err
	}))
}

func (f *reconcileCase) subscription(ref, status, email string, customer uuid.UUID) LocalSubscription {
	f.t.Helper()
	if customer == uuid.Nil {
		customer = uuid.New()
	}
	product, price := uuid.New(), uuid.New()
	s := LocalSubscription{ID: uuid.New(), CustomerID: customer, ProductID: product, PriceID: &price,
		Rail: string(f.snap.Provider), RailSubscriptionID: ref, Status: status, UserEmail: email,
		StartedAt: f.now.Add(-100 * 24 * time.Hour), CurrentPeriodStartsAt: tp(f.now.Add(-10 * 24 * time.Hour)), CurrentPeriodEndsAt: tp(f.now.Add(20 * 24 * time.Hour)), EntitlementNames: []string{"premium"}}
	if status == "cancelled" {
		s.CancelledAt = tp(f.now.Add(-3 * 24 * time.Hour))
		s.CancelType = "user"
	}
	f.exec(`INSERT INTO openrails.customers (merchant_id,id,issuer) VALUES ($1,$2,'reconcile-case') ON CONFLICT DO NOTHING`, f.merchant.UUID(), customer)
	f.exec(`INSERT INTO openrails.products (id,merchant_id,key,display_name,entitlements_spec) VALUES ($1,$2,$3,'Premium','{"premium":null}')`, product, f.merchant.UUID(), "case-"+product.String())
	f.exec(`INSERT INTO openrails.prices (id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew) VALUES ($1,$2,$3,9990000,'USD',720,true)`, price, f.merchant.UUID(), product)
	f.exec(`INSERT INTO openrails.subscriptions (id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,user_email,started_at,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot,psp_id,cancelled_at,cancel_type)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'{"premium":null}',$13,$14,NULLIF($15,''))`, s.ID, f.merchant.UUID(), customer, product, price, status, s.Rail, ref, email, s.StartedAt, s.CurrentPeriodStartsAt, s.CurrentPeriodEndsAt, f.psp.ID, s.CancelledAt, s.CancelType)
	f.snap.Subscriptions = append(f.snap.Subscriptions, RemoteSubscription{RailSubscriptionID: ref, Status: SubscriptionStatus(status), NextBillingAt: s.CurrentPeriodEndsAt})
	return s
}

func (f *reconcileCase) grant(s LocalSubscription) {
	f.t.Helper()
	f.exec(`INSERT INTO openrails.entitlements (merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type) VALUES ($1,$2,'premium',$3,$4,$5,'subscription')`, f.merchant.UUID(), s.CustomerID, s.CurrentPeriodStartsAt, s.CurrentPeriodEndsAt, s.ID)
}

func (f *reconcileCase) payment(s LocalSubscription, txn, status string, amount int64, refunded *uuid.UUID, at time.Time) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO openrails.payments (id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,subscription_id,refunded_payment_id,purchased_at,psp_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,'USD',$8,$9,$10,$11,$12)`, id, f.merchant.UUID(), s.CustomerID, s.PriceID, s.Rail, txn, amount, status, s.ID, refunded, at, f.psp.ID)
	return id
}

func (f *reconcileCase) method(customer uuid.UUID, ref string) {
	f.t.Helper()
	f.exec(`INSERT INTO openrails.payment_methods (merchant_id,customer_id,psp_id,rail,initial_transaction_id,rail_customer_ref,last_four,expiry_date) VALUES ($1,$2,$3,$4,'initial',$5,'1111','1029')`, f.merchant.UUID(), customer, f.psp.ID, f.psp.Rail, ref)
}

func (f *reconcileCase) run(mode Mode) *RunResult {
	f.t.Helper()
	var result *RunResult
	require.NoError(f.t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		var err error
		result, err = f.engine.Run(ctx, RunParams{Mode: mode, Providers: []Provider{f.snap.Provider}, PSPs: map[Provider]PSPBinding{f.snap.Provider: f.psp}})
		return err
	}))
	return result
}

func (f *reconcileCase) finding(result *RunResult, kind FindingType) FindingRecord {
	f.t.Helper()
	matches := findByType(result.Findings, kind)
	require.Len(f.t, matches, 1)
	var stored FindingRecord
	require.NoError(f.t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		var err error
		stored, err = (&PGStore{DB: f.db}).GetFinding(ctx, matches[0].ID)
		return err
	}))
	require.Equal(f.t, matches[0].Status, stored.Status, "returned and persisted finding disagree")
	return stored
}

func (f *reconcileCase) state() string {
	f.t.Helper()
	return reconcileBillingState(f.t, f.db, f.ctx)
}

func TestReconcileTaxonomyWorkflow(t *testing.T) {
	t.Run("PS1 unknown identity is held", func(t *testing.T) {
		f := newReconcileCase(t, ProviderNMI)
		s := f.subscription("known", "active", "", uuid.Nil)
		f.grant(s)
		f.snap.Subscriptions = append(f.snap.Subscriptions, RemoteSubscription{RailSubscriptionID: "ghost", Status: SubscriptionStatusActive, Email: "ghost@example.com"})
		before := f.state()
		finding := f.finding(f.run(ModeEnforce), FindingRemoteSubMissingLocal)
		require.Equal(t, "ghost", finding.SubjectKey)
		require.Equal(t, SeverityCritical, finding.Severity)
		require.Equal(t, FindingStatusAdminRequired, finding.Status)
		require.True(t, finding.RequiresAdmin)
		require.Equal(t, before, f.state())
	})
	t.Run("PS1 email candidate is not an identity proof", func(t *testing.T) {
		f := newReconcileCase(t, ProviderNMI)
		s := f.subscription("known", "active", "jane@example.com", uuid.Nil)
		f.snap.Subscriptions = append(f.snap.Subscriptions, RemoteSubscription{RailSubscriptionID: "ghost", Status: SubscriptionStatusActive, Email: "JANE@example.com"})
		before := f.state()
		finding := f.finding(f.run(ModeAdvisory), FindingRemoteSubMissingLocal)
		candidates, ok := finding.LocalEvidence["email_candidates"].([]any)
		require.True(t, ok)
		require.Len(t, candidates, 1)
		require.Equal(t, openrails.SubscriptionID(s.ID).String(), candidates[0].(map[string]any)["subscription_id"])
		require.Equal(t, FindingStatusAdminRequired, finding.Status)
		require.Equal(t, before, f.state())
	})
	t.Run("PS2 NMI exhaustive absence cancels", func(t *testing.T) {
		f := newReconcileCase(t, ProviderNMI)
		dead := f.subscription("dead", "active", "", uuid.Nil)
		f.grant(dead)
		alive := f.subscription("alive", "active", "", uuid.Nil)
		f.grant(alive)
		f.snap.Subscriptions = f.snap.Subscriptions[1:]
		f.snap.Coverage.SubscriptionsExhaustive = true
		result := f.run(ModeEnforce)
		finding := f.finding(result, FindingLocalActiveRemoteDead)
		require.Equal(t, dead.ID.String(), finding.SubjectKey)
		require.Equal(t, SeverityHigh, finding.Severity)
		require.Equal(t, FindingStatusAutoFixed, finding.Status)
		require.Equal(t, "enforced", finding.Resolution)
		require.Equal(t, 1, result.Summary.Providers["nmi"].AutoFixed)
		require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
			local, err := (&PGLocalStateLoader{DB: f.db}).Load(ctx, ProviderNMI, f.psp.ID)
			require.NoError(t, err)
			for _, sub := range local.Subscriptions {
				if sub.ID == dead.ID {
					require.Equal(t, "cancelled", sub.Status)
					require.Equal(t, "expired", sub.CancelType)
				}
			}
			return nil
		}))
	})
	t.Run("PS2 Stripe requires a present terminal record", func(t *testing.T) {
		f := newReconcileCase(t, ProviderStripe)
		f.subscription("sub_123", "active", "", uuid.Nil)
		f.snap.Subscriptions[0].Status = SubscriptionStatusCancelled
		before := f.state()
		finding := f.finding(f.run(ModeAdvisory), FindingLocalActiveRemoteDead)
		require.Equal(t, FindingStatusReconcileRequired, finding.Status)
		require.Equal(t, before, f.state())
	})
	t.Run("PS2 empty CCBill export proves no absence", func(t *testing.T) {
		f := newReconcileCase(t, ProviderCCBill)
		f.subscription("920000001", "active", "", uuid.Nil)
		f.snap.Subscriptions = nil
		f.snap.Capabilities = Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true}
		before := f.state()
		require.Empty(t, findByType(f.run(ModeAdvisory).Findings, FindingLocalActiveRemoteDead))
		require.Equal(t, before, f.state())
	})
	t.Run("PS3 provider status and periods converge", func(t *testing.T) {
		f := newReconcileCase(t, ProviderStripe)
		s := f.subscription("sub_42", "past_due", "", uuid.Nil)
		end := f.now.Add(25 * 24 * time.Hour)
		f.snap.Subscriptions[0] = RemoteSubscription{RailSubscriptionID: s.RailSubscriptionID, Status: SubscriptionStatusActive, RawStatus: "active", LastBilledAt: tp(f.now.Add(-5 * 24 * time.Hour)), NextBillingAt: &end}
		result := f.run(ModeEnforce)
		finding := f.finding(result, FindingStatusMismatch)
		require.Equal(t, FindingStatusAutoFixed, finding.Status)
		require.Len(t, result.PlannedChanges, 1)
		require.Len(t, result.AppliedChanges, 1)
		for _, mutation := range []MutationRecord{result.PlannedChanges[0], result.AppliedChanges[0]} {
			require.Equal(t, "subscriptions", mutation.Table)
			require.Equal(t, "update", mutation.Operation)
			require.Equal(t, s.ID.String(), mutation.RowID)
		}
		require.Equal(t, "planned", result.PlannedChanges[0].Phase)
		require.Equal(t, "applied", result.AppliedChanges[0].Phase)
		require.Equal(t, 1, result.AppliedChanges[0].RowsAffected)
		require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
			local, err := (&PGLocalStateLoader{DB: f.db}).Load(ctx, ProviderStripe, f.psp.ID)
			require.NoError(t, err)
			require.Len(t, local.Subscriptions, 1)
			require.Equal(t, "active", local.Subscriptions[0].Status)
			require.NotNil(t, local.Subscriptions[0].CurrentPeriodEndsAt)
			require.True(t, end.Equal(*local.Subscriptions[0].CurrentPeriodEndsAt))
			return nil
		}))
	})
	t.Run("PS3 user cancellation is never resurrected", func(t *testing.T) {
		f := newReconcileCase(t, ProviderStripe)
		s := f.subscription("sub_dead", "cancelled", "", uuid.Nil)
		f.exec(`UPDATE openrails.subscriptions SET cancel_type='user',cancelled_at=$2 WHERE id=$1`, s.ID, f.now.Add(-3*24*time.Hour))
		f.snap.Subscriptions[0].Status = SubscriptionStatusActive
		before := f.state()
		finding := f.finding(f.run(ModeEnforce), FindingStatusMismatch)
		require.Equal(t, FindingStatusAdminRequired, finding.Status)
		require.True(t, finding.RequiresAdmin)
		require.Empty(t, finding.IntentEvidence)
		require.Equal(t, before, f.state())
	})
	t.Run("PS4 correlated charge backfills its payment and access", func(t *testing.T) {
		f := newReconcileCase(t, ProviderNMI)
		s := f.subscription("nmi-sub-1", "active", "", uuid.Nil)
		f.snap.Capabilities.Transactions = true
		f.snap.Capabilities.Refunds = true
		txn := nmiSaleTxn("txn-1001", fmt.Sprintf("rebill-%s-%d", s.ID, f.now.Unix()), true)
		txn.OccurredAt = f.now.Add(-48 * time.Hour)
		f.snap.Transactions = []RemoteTransaction{txn}
		finding := f.finding(f.run(ModeEnforce), FindingChargeMissingLocal)
		require.Equal(t, "txn-1001", finding.SubjectKey)
		require.Equal(t, SeverityHigh, finding.Severity)
		require.Equal(t, FindingStatusAutoFixed, finding.Status)
		require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
			payments, err := (&PGLocalStateLoader{DB: f.db}).PaymentsByTransactionIDs(ctx, ProviderNMI, f.psp.ID, []string{"txn-1001"})
			require.NoError(t, err)
			require.Len(t, payments, 1)
			require.Equal(t, s.CustomerID, payments[0].CustomerID)
			require.EqualValues(t, 999, payments[0].AmountCents)
			var entitlement string
			require.NoError(t, f.db.Qx(ctx).QueryRow(ctx, `SELECT entitlement FROM openrails.entitlements WHERE customer_id=$1`, s.CustomerID).Scan(&entitlement))
			require.Equal(t, "premium", entitlement)
			return nil
		}))
	})
	t.Run("PS4 ambiguous customer is never guessed", func(t *testing.T) {
		f := newReconcileCase(t, ProviderNMI)
		f.subscription("nmi-a", "active", "dup@example.com", uuid.Nil)
		f.subscription("nmi-b", "active", "dup@example.com", uuid.Nil)
		f.snap.Capabilities.Transactions = true
		f.snap.Transactions = []RemoteTransaction{{TransactionID: "txn-amb", Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: f.now.Add(-time.Hour), Raw: rawJSON(map[string]any{"email": "dup@example.com"})}}
		before := f.state()
		finding := f.finding(f.run(ModeEnforce), FindingChargeMissingLocal)
		require.Equal(t, FindingStatusAdminRequired, finding.Status)
		require.True(t, finding.RequiresAdmin)
		require.Contains(t, finding.RecommendedAction, "MULTIPLE")
		require.Equal(t, before, f.state())
	})
	t.Run("PS5 NMI refund marks original without duplicating it", func(t *testing.T) {
		f := newReconcileCase(t, ProviderNMI)
		s := f.subscription("nmi-sub-5", "active", "", uuid.Nil)
		f.payment(s, "txn-5001", "completed", 9_990_000, nil, f.now.Add(-10*24*time.Hour))
		f.snap.Capabilities.Transactions = true
		f.snap.Capabilities.Refunds = true
		f.snap.Transactions = []RemoteTransaction{{TransactionID: "txn-5001", Type: TransactionTypeRefund, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: f.now.Add(-time.Hour), Raw: rawJSON(map[string]any{"order_id": s.ID.String()})}}
		finding := f.finding(f.run(ModeEnforce), FindingRefundUnrecorded)
		require.Equal(t, FindingStatusAutoFixed, finding.Status)
		require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
			payments, err := (&PGLocalStateLoader{DB: f.db}).PaymentsByTransactionIDs(ctx, ProviderNMI, f.psp.ID, []string{"txn-5001"})
			require.NoError(t, err)
			require.Len(t, payments, 1)
			require.Equal(t, "refunded", payments[0].Status)
			return nil
		}))
	})
	t.Run("PS5 recorded Stripe refund is quiet", func(t *testing.T) {
		f := newReconcileCase(t, ProviderStripe)
		s := f.subscription("sub_55", "active", "", uuid.Nil)
		original := f.payment(s, "ch_1", "refunded", 9_990_000, nil, f.now.Add(-9*24*time.Hour))
		f.payment(s, "re_1", "completed", -9_990_000, &original, f.now.Add(-8*24*time.Hour))
		f.snap.Capabilities = Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true}
		f.snap.Transactions = []RemoteTransaction{{TransactionID: "re_1", Type: TransactionTypeRefund, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: f.now.Add(-8 * 24 * time.Hour), Raw: rawJSON(map[string]any{"charge": "ch_1"})}}
		before := f.state()
		require.Empty(t, findByType(f.run(ModeAdvisory).Findings, FindingRefundUnrecorded))
		require.Equal(t, before, f.state())
	})
	t.Run("PS6 chargeback is a critical operator decision", func(t *testing.T) {
		f := newReconcileCase(t, ProviderStripe)
		s := f.subscription("sub_66", "active", "", uuid.Nil)
		f.grant(s)
		f.payment(s, "ch_66", "completed", 9_990_000, nil, f.now.Add(-5*24*time.Hour))
		f.snap.Capabilities = Capabilities{Subscriptions: true, Transactions: true, Refunds: true, Chargebacks: true}
		f.snap.Transactions = []RemoteTransaction{{TransactionID: "dp_1", Type: TransactionTypeChargeback, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: f.now.Add(-time.Hour), Raw: rawJSON(map[string]any{"charge": "ch_66"})}}
		before := f.state()
		finding := f.finding(f.run(ModeEnforce), FindingChargebackActiveSub)
		require.Equal(t, SeverityCritical, finding.Severity)
		require.Equal(t, FindingStatusAdminRequired, finding.Status)
		require.True(t, finding.RequiresAdmin)
		require.Equal(t, before, f.state())
	})
	t.Run("PS8 duplicate tier ownership needs review", func(t *testing.T) {
		f := newReconcileCase(t, ProviderNMI)
		s := f.subscription("dup-1", "active", "", uuid.Nil)
		other := f.subscription("dup-2", "past_due", "", s.CustomerID)
		f.exec(`UPDATE openrails.subscriptions SET tier_group='premium' WHERE id=ANY($1)`, []uuid.UUID{s.ID, other.ID})
		f.snap.Subscriptions[1].Status = SubscriptionStatusActive
		finding := f.finding(f.run(ModeEnforce), FindingDuplicateSubscriptions)
		require.Equal(t, FindingStatusAdminRequired, finding.Status)
		require.True(t, finding.RequiresAdmin)
		// The mismatch may repair the second row's status, but neither row may be cancelled.
		require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
			var cancelled int
			require.NoError(t, f.db.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.subscriptions WHERE status='cancelled'`).Scan(&cancelled))
			require.Zero(t, cancelled)
			return nil
		}))
	})
}

func TestReconcileIntentAndIgnoredFinding(t *testing.T) {
	t.Run("recorded delete keeps status drift out of the operator queue", func(t *testing.T) {
		f := newReconcileCase(t, ProviderNMI)
		s := f.subscription("nmi-intent", "cancelled", "", uuid.Nil)
		f.exec(`UPDATE openrails.subscriptions SET cancel_type='user',cancelled_at=$2,deletion_scheduled_at=$3 WHERE id=$1`, s.ID, f.now.Add(-24*time.Hour), f.now.Add(12*time.Hour))
		f.snap.Subscriptions[0].Status = SubscriptionStatusActive
		before := f.state()
		finding := f.finding(f.run(ModeEnforce), FindingStatusMismatch)
		require.Equal(t, FindingStatusReconcileRequired, finding.Status)
		require.False(t, finding.RequiresAdmin)
		require.NotNil(t, finding.IntentEvidence)
		require.Contains(t, finding.IntentEvidence["explanation"], "intent executor")
		require.Equal(t, deletionIntentAction, finding.RecommendedAction)
		require.Equal(t, before, f.state())
	})
	t.Run("operator ignored identity stays ignored after enforce", func(t *testing.T) {
		f := newReconcileCase(t, ProviderStripe)
		s := f.subscription("sub_dismiss", "active", "", uuid.Nil)
		f.grant(s)
		f.snap.Subscriptions[0].Status = SubscriptionStatusCancelled
		first := f.finding(f.run(ModeAdvisory), FindingLocalActiveRemoteDead)
		require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
			changed, err := (&PGStore{DB: f.db}).IgnoreFindingWithActor(ctx, first.ID, "reviewed", "operator")
			require.NoError(t, err)
			require.True(t, changed)
			return nil
		}))
		before := f.state()
		finding := f.finding(f.run(ModeEnforce), FindingLocalActiveRemoteDead)
		require.Equal(t, first.ID, finding.ID)
		require.Equal(t, FindingStatusIgnored, finding.Status)
		require.Equal(t, before, f.state())
	})
}

func TestReconcileProviderCapabilities(t *testing.T) {
	f := newReconcileCase(t, ProviderNMI)
	s := f.subscription("nmi-gate", "active", "", uuid.Nil)
	f.method(s.CustomerID, "vault-gate")
	f.payment(s, "txn-gate", "completed", 9_990_000, nil, f.now.Add(-time.Hour))
	f.snap.Capabilities.Transactions = true
	f.snap.Transactions = []RemoteTransaction{
		{TransactionID: "cb-1", Type: TransactionTypeChargeback, Success: true, AmountCents: 999, OccurredAt: f.now.Add(-time.Hour), Raw: rawJSON(map[string]any{"order_id": s.ID.String()})},
		{TransactionID: "rf-1", Type: TransactionTypeRefund, Success: true, AmountCents: 999, OccurredAt: f.now.Add(-time.Hour), Raw: rawJSON(map[string]any{"order_id": s.ID.String()})},
	}
	f.snap.PaymentMethods = []RemotePaymentMethod{{RailCustomerRef: "vault-gate", CardLast4: "9999", CardExpiry: "1299"}}
	before := f.state()
	result := f.run(ModeAdvisory)
	for _, kind := range []FindingType{FindingChargebackActiveSub, FindingRefundUnrecorded, FindingPaymentMethodMismatch} {
		require.Empty(t, findByType(result.Findings, kind), kind)
	}
	require.Equal(t, before, f.state())
}

func TestReconcileMaterializationRefusals(t *testing.T) {
	for _, scenario := range []string{"advisory", "ambiguous identity", "unresolved plan", "past due without period"} {
		t.Run(scenario, func(t *testing.T) {
			f := newReconcileCase(t, ProviderNMI)
			owner := f.subscription("known", "active", "", uuid.Nil)
			f.exec(`DELETE FROM openrails.subscriptions WHERE id=$1`, owner.ID)
			f.snap.Subscriptions = nil
			f.method(owner.CustomerID, "vault-77")
			f.exec(`INSERT INTO openrails.price_psp_bindings (merchant_id,price_id,psp_id,plan_id) VALUES ($1,$2,$3,'plan-gold')`, f.merchant.UUID(), owner.PriceID, f.psp.ID)
			f.snap.Capabilities = Capabilities{Subscriptions: true, Transactions: true, Vault: true}
			remote := RemoteSubscription{RailSubscriptionID: "remote-77", Status: SubscriptionStatusActive, CustomerID: "vault-77", Email: "owner@example.com", PlanID: "plan-gold", NextBillingAt: tp(f.now.Add(20 * 24 * time.Hour)), LastBilledAt: tp(f.now.Add(-10 * 24 * time.Hour)), AmountCents: 999, Currency: "USD"}
			f.snap.Transactions = []RemoteTransaction{{TransactionID: "txn-mat-1", Type: TransactionTypeSale, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: f.now.Add(-10 * 24 * time.Hour), Raw: rawJSON(map[string]any{"customer_vault_id": "vault-77"})}}
			f.snap.PaymentMethods = []RemotePaymentMethod{{RailCustomerRef: "vault-77", CardLast4: "1111", CardExpiry: "1029"}}
			mode := ModeEnforce
			blocker := ""
			switch scenario {
			case "advisory":
				mode = ModeAdvisory
			case "ambiguous identity":
				f.subscription("other", "active", "owner@example.com", uuid.Nil)
				blocker = "ambiguous"
			case "unresolved plan":
				f.exec(`DELETE FROM openrails.price_psp_bindings WHERE price_id=$1`, owner.PriceID)
				blocker = "plan unresolved"
			case "past due without period":
				remote.Status = SubscriptionStatusPastDue
				remote.NextBillingAt = nil
				blocker = "past_due"
			}
			f.snap.Subscriptions = append(f.snap.Subscriptions, remote)
			before := f.state()
			finding := f.finding(f.run(mode), FindingRemoteSubMissingLocal)
			require.Equal(t, FindingStatusAdminRequired, finding.Status)
			require.True(t, finding.RequiresAdmin)
			if blocker != "" {
				require.Contains(t, finding.RemoteEvidence["materialize_blocked"], blocker)
			}
			require.Equal(t, before, f.state())
		})
	}
}

func TestReconcileHistorySourceWorkflow(t *testing.T) {
	f := newReconcileCase(t, ProviderNMI)
	s := f.subscription("nmi-hist", "past_due", "", uuid.Nil)
	f.snap.Capabilities.Transactions = true
	histAt := f.now.Add(-200 * 24 * time.Hour)
	f.payment(s, "failed-history", "failed", 9_990_000, nil, histAt)
	f.engine.History = NewPGHistorySource(f.db)
	before := f.state()
	result := f.run(ModeAdvisory)
	history := result.Summary.Providers["nmi"].Dunning
	require.NotNil(t, history)
	require.Equal(t, "ok: 1 events (1 correlated)", history.HistorySource)
	require.Len(t, history.Details, 1)
	line := history.Details[0]
	require.Equal(t, 1, line.HistoryEvents)
	require.Equal(t, 1, line.HistoryFailures)
	require.Equal(t, 0, line.HistorySuccesses)
	require.Equal(t, "history", history.LastDunningActionVia)
	require.Equal(t, histAt, *history.LastDunningActionAnySource)
	require.Equal(t, "never_attempted", line.Classification)
	require.Equal(t, 1, history.NeverAttempted)
	require.Equal(t, before, f.state())
	closed, err := pgxpool.NewWithConfig(t.Context(), f.db.Pool().Config().Copy())
	require.NoError(t, err)
	unavailable, err := db.NewWithPGXPool(closed, "")
	require.NoError(t, err)
	closed.Close()
	for name, source := range map[string]HistoryEventSource{"unavailable": NewPGHistorySource(unavailable), "not configured": NewPGHistorySource(nil)} {
		t.Run(name, func(t *testing.T) {
			f.engine.History = source
			res := f.run(ModeAdvisory)
			require.Equal(t, "completed", res.Status)
			require.Contains(t, res.Summary.Providers["nmi"].Dunning.HistorySource, name)
			require.Equal(t, before, f.state())
		})
	}
}

func TestReconcileCancellationBudget(t *testing.T) {
	for _, remoteCount := range []int{30, 197} {
		t.Run(fmt.Sprintf("%d_of_200", remoteCount), func(t *testing.T) {
			f := newReconcileCase(t, ProviderNMI)
			for i := range 200 {
				s := f.subscription(fmt.Sprintf("nmi-%d", i), "active", "", uuid.Nil)
				f.grant(s)
			}
			f.snap.Subscriptions = f.snap.Subscriptions[:remoteCount]
			f.snap.Coverage.SubscriptionsExhaustive = true
			before := f.state()
			var result *RunResult
			require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
				var err error
				result, err = f.engine.Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}, PSPs: map[Provider]PSPBinding{ProviderNMI: f.psp}})
				if remoteCount == 30 {
					require.ErrorContains(t, err, "cancellation cap")
				} else {
					require.NoError(t, err)
				}
				return nil
			}))
			if remoteCount == 30 {
				require.True(t, result.Summary.Providers["nmi"].Aborted)
				require.Len(t, findByType(result.Findings, FindingLocalActiveRemoteDead), 170)
				finding := f.finding(result, FindingCancellationCapped)
				require.Equal(t, "cancellation_cap", finding.SubjectKey)
				require.Equal(t, FindingStatusRequiresReview, finding.Status)
				require.Equal(t, before, f.state(), "the cap must withhold every cancellation and access change")
			} else {
				require.False(t, result.Summary.Providers["nmi"].Aborted)
				require.Equal(t, 3, result.Summary.Providers["nmi"].AutoFixed)
				require.NoError(t, f.db.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
					var cancelled int
					require.NoError(t, f.db.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.subscriptions WHERE status='cancelled'`).Scan(&cancelled))
					require.Equal(t, 3, cancelled)
					return nil
				}))
			}
		})
	}
}
