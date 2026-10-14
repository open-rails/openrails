package migrate

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
)

// A submitted old enrollment may first acquire its positive receipts after the
// provider-price migration. Only its subscription projection moves; neither
// the signed operation nor its bill/payment/access history is rewritten.
func TestAcceptedNativeEnrollmentRepairsAcrossProviderPriceMigration(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("OPENRAILS_E2E_DSN")
	}
	if dsn == "" {
		t.Skip("a disposable PostgreSQL database is required")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	for _, tc := range []struct {
		name, plan           string
		binding              bool
		amount               int64
		cadence              int
		alreadyProviderOwned bool
		wantSuccess          bool
	}{
		{name: "archived accepted account", binding: true, plan: "accepted-plan", amount: 23_000_000, wantSuccess: true},
		{name: "already provider owned", binding: true, plan: "accepted-plan", amount: 23_000_000, alreadyProviderOwned: true, wantSuccess: true},
		{name: "no accepted account mapping", plan: "accepted-plan", amount: 23_000_000},
		{name: "another plan", binding: true, plan: "different-plan", amount: 23_000_000},
		{name: "different frozen amount", binding: true, plan: "accepted-plan", amount: 29_000_000},
		{name: "different frozen cadence", binding: true, plan: "accepted-plan", amount: 23_000_000, cadence: 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			schema := "native_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			t.Cleanup(func() {
				_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
				require.NoError(t, err)
			})
			migrations, err := loadMigrations(schema)
			require.NoError(t, err)
			at := -1
			for i, m := range migrations {
				if strings.HasPrefix(m.Name, "0104_") {
					at = i
				}
			}
			require.Positive(t, at)
			migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
			require.NoError(t, err)
			defer migrator.Close()
			migrator.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
			require.NoError(t, migrator.ApplyMigrations(ctx, migrations[:at]))
			query := func(sql string, args ...any) pgx.Row {
				return pool.QueryRow(ctx, strings.ReplaceAll(sql, "billing.", schema+"."), args...)
			}
			exec := func(sql string, args ...any) {
				t.Helper()
				_, err := pool.Exec(ctx, strings.ReplaceAll(sql, "billing.", schema+"."), args...)
				require.NoError(t, err)
			}
			mid, product, original, oldPSP, currentPSP, customer := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			acceptedAt := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
			end := acceptedAt.Add(30 * 24 * time.Hour)
			exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,$2,'active')`, mid, "native-repair-"+mid.String())
			exec(`INSERT INTO billing.products(merchant_id,id,key,display_name,tier_rank,archived) VALUES($1,$2,'membership','Membership',0,false)`, mid, product)
			exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
			// The current account deliberately has the same label and plan. It is
			// never authority for the old accepted account's repaired membership.
			exec(`INSERT INTO billing.psps(merchant_id,id,key,rail,environment,account_id,archived,superseded_at) VALUES($1,$2,'nmi','nmi','sandbox','accepted-account',true,$3)`, mid, oldPSP, acceptedAt)
			exec(`INSERT INTO billing.psps(merchant_id,id,key,rail,environment,account_id,archived) VALUES($1,$2,'nmi','nmi','sandbox','current-account',false)`, mid, currentPSP)
			exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived,access_duration_hours,billing_interval_hours) VALUES($1,$2,$3,'monthly',23000000,'USD',true,720,720)`, mid, original, product)
			exec(`INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,plan_id,configuration) VALUES($1,$2,$3,'accepted-plan','{}')`, mid, original, currentPSP)
			if tc.binding {
				exec(`INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,plan_id,configuration) VALUES($1,$2,$3,'accepted-plan','{}')`, mid, original, oldPSP)
			}
			canonical := uuid.UUID(md5.Sum([]byte(original.String() + ":" + oldPSP.String())))
			acceptedPrice := original
			if tc.alreadyProviderOwned {
				require.NoError(t, migrator.ApplyMigrations(ctx, migrations))
				acceptedPrice = canonical
			}
			clock := clockwork.NewFakeClockAt(acceptedAt.Add(time.Hour))
			gateway := nmimock.New(nmimock.Options{Clock: clock.Now})
			t.Cleanup(gateway.Close)
			vault := gateway.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
			instrument := gateway.Vault(vault)
			operation := uuid.New()
			payload := subscriptions.InitialMembershipPayload{
				Terms: subscriptions.InitialMembershipTerms{
					CollectionPolicy: models.CollectionPolicyNMISchedule,
					SubscriptionID:   uuid.New(), PaymentID: uuid.New(), CustomerID: customer,
					PSPID: oldPSP, ProductID: product, PriceID: acceptedPrice,
					ProductName: "Membership", RecurringIntervalHours: new(720), AccessDurationHours: new(720),
					Amount: tc.amount, RecurringAmount: tc.amount, Currency: "USD",
					AcceptedAt: acceptedAt, PeriodStart: acceptedAt, PeriodEnd: end,
				},
				Instrument: charge.FrozenInstrument{PSPID: oldPSP, Custodian: models.CustodianPSP, RailCustomerRef: vault, RailMethodRef: instrument.BillingID},
				PSP:        "nmi", RequestFingerprint: strings.Repeat("a", 64), PurchaseIdempotencyKey: operation.String(),
				NativeSchedule: &subscriptions.NMIInitialScheduleTerms{PlanID: tc.plan, DayFrequency: 30, StartDate: end.Format("20060102")},
			}
			if tc.cadence != 0 {
				payload.Terms.RecurringIntervalHours = &tc.cadence
			}
			wire, err := json.Marshal(payload)
			require.NoError(t, err)
			exec(`INSERT INTO billing.provider_intents(id,merchant_id,rail,intent_type,price_id,psp_id,payload,idempotency_key,status,next_attempt_at,origin,attempts,result_evidence) VALUES($1,$2,'nmi','initial_membership',$3,$4,$5,$6,'in_flight',$7,'user',1,'{"initial_submitted":true}')`, operation, mid, acceptedPrice, oldPSP, wire, payment.InitialMembershipIdempotencyKey(operation.String()), acceptedAt)
			var acceptedSnapshot string
			require.NoError(t, query(`SELECT payload::text FROM billing.provider_intents WHERE id=$1`, operation).Scan(&acceptedSnapshot))
			if !tc.alreadyProviderOwned {
				require.NoError(t, migrator.ApplyMigrations(ctx, migrations))
			}
			database, err := db.NewWithPGXPool(pool, schema)
			require.NoError(t, err)
			ctx = db.WithPSPID(merchant.WithID(ctx, billing.MerchantID(mid)), oldPSP)
			in, err := intents.NewStore(database).Get(ctx, operation)
			require.NoError(t, err)
			_, err = subscriptions.DecodeInitialMembershipPayload(in)
			require.NoError(t, err)
			orderRef := intents.NMIEnrollmentOrder(in)
			amount := fmt.Sprintf("%d.00", tc.amount/1_000_000)
			gateway.AddPlan(nmimock.Plan{ID: tc.plan, Name: "Membership", Amount: amount, Days: 30})
			remote := gateway.AddSchedule(nmimock.Schedule{Vault: vault, Plan: tc.plan, Amount: amount, Order: orderRef, Days: 30, NextBilling: end, Created: acceptedAt})
			gateway.AddSale(nmimock.Sale{OrderID: orderRef, Vault: vault, BillingID: instrument.BillingID, Amount: amount, Currency: "USD", At: acceptedAt})
			client, err := nmi.NewAccountClient(mid, oldPSP, "nmi", &config.NMIProviderSettings{SecurityKey: "native-repair-fixture", Environment: config.EnvironmentSandbox})
			require.NoError(t, err)
			client.UseTransport(gateway)
			makeHandler := func() *payment.InitialMembershipIntentHandler {
				// A fresh service graph models a restarted worker reading the
				// persisted intent and its retained receipts, never current keys.
				d, err := db.NewWithPGXPool(pool, schema)
				require.NoError(t, err)
				prices, products := catalog.NewPriceService(d), catalog.NewProductService(d)
				paid := payments.NewPaymentService(d, clock)
				access := entitlements.NewEntitlementService(d, clock)
				notices := subscriptions.NewNotificationService(d)
				subs := subscriptions.NewSubscriptionService(d, prices, products, nil, clock)
				purchases := payment.NewPurchaseService(subs, products, prices, paid, access, nil, nil, nil, &config.Config{}, nil, clock)
				purchases.Lifecycle = subscriptions.NewSubscriptionLifecycleService(d, products, prices, access, notices, paid, clock)
				purchases.ResolveNMIClientOverride = func(call context.Context, _ string) (*nmi.NMIClient, error) {
					if db.PSPIDFromContext(call) != oldPSP {
						return nil, errors.New("receipt selected another account")
					}
					return client, nil
				}
				return payment.NewInitialMembershipIntentHandler(purchases)
			}
			outcome := makeHandler().Verify(ctx, in)
			if !tc.wantSuccess {
				require.Equal(t, intents.OutcomeAmbiguous, outcome.Class, outcome.Reason)
				var receipt, schedule bool
				require.NoError(t, query(`SELECT result_evidence ? 'qualified_receipt',result_evidence ? 'qualified_enrollment' FROM billing.provider_intents WHERE id=$1`, operation).Scan(&receipt, &schedule))
				require.True(t, receipt, "the actual payment's positive custody survives refused local projection")
				require.True(t, schedule, "the positive native schedule receipt survives refused projection")
				var count int
				require.NoError(t, query(`SELECT count(*) FROM billing.subscriptions`).Scan(&count))
				require.Zero(t, count)
			} else {
				require.Equal(t, intents.OutcomeSucceeded, outcome.Class, outcome.Reason)
				var price, psp, orderPrice uuid.UUID
				var provider, policy string
				require.NoError(t, query(`SELECT price_id,psp_id,rail_subscription_id,collection_policy FROM billing.subscriptions WHERE id=$1`, payload.Terms.SubscriptionID).Scan(&price, &psp, &provider, &policy))
				require.Equal(t, canonical, price)
				require.Equal(t, oldPSP, psp, "superseded account keeps ownership")
				require.Equal(t, remote, provider)
				require.Equal(t, "nmi_schedule", policy)
				require.NoError(t, query(`SELECT l.price_id FROM billing.payments p JOIN billing.order_lines l ON l.merchant_id=p.merchant_id AND l.order_id=p.order_id WHERE p.id=$1`, payload.Terms.PaymentID).Scan(&orderPrice))
				require.Equal(t, acceptedPrice, orderPrice, "original accepted bill identity never follows the catalog projection")
				var start, finish time.Time
				require.NoError(t, query(`SELECT starts_at,ends_at FROM billing.product_access WHERE source_id=$1`, payload.Terms.SubscriptionID.String()).Scan(&start, &finish))
				require.Equal(t, acceptedAt, start)
				require.Equal(t, end, finish)
				require.Equal(t, intents.OutcomeSucceeded, makeHandler().Verify(ctx, in).Class, "restarted replay uses retained receipts")
				for _, table := range []string{"subscriptions", "payments", "orders"} {
					var count int
					require.NoError(t, query("SELECT count(*) FROM billing."+table).Scan(&count))
					require.Equal(t, 1, count, table+" is never duplicated")
				}
			}
			var after string
			require.NoError(t, query(`SELECT payload::text FROM billing.provider_intents WHERE id=$1`, operation).Scan(&after))
			require.Equal(t, acceptedSnapshot, after, "signed payload is never rewritten")
			require.Empty(t, gateway.Calls(), "recovery only reads existing provider effects")
			require.Empty(t, gateway.Unexpected())
		})
	}
}
