//go:build integration

package subscriptions

// #815 gateway-native NMI auto-migration suite — exercises the REAL
// nmiPlanPusher (classic Direct Post update_subscription + v5 GET read-back)
// against a scripted fake NMI gateway, mirroring the intents package's
// fake-gateway pattern. Covers: the boundary push end-to-end (wire shape,
// plan_payments preservation, internal cutover, money invariants), the
// early-flip guard + final-period re-run, push-failure and verify-mismatch
// degradation, idempotent re-runs, interval-mismatch blocking, and
// preview/execute parity.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// fakeNMIPlanGateway scripts the two endpoints the #815 push touches: the v5
// subscription GET (read + verify) and the classic Direct Post
// recurring=update_subscription write.
type fakeNMIPlanGateway struct {
	railSubID    string
	amount       atomic.Value // decimal string NMI currently bills ("10.00")
	planPayments string       // remote plan_payments the push must preserve
	updateMode   atomic.Value // "ok" | "rejected" | "stale" (accepted, amount unchanged)
	getCalls     atomic.Int64
	updateCalls  atomic.Int64
	updateForms  []map[string]string
}

func newFakeNMIPlanGateway(t *testing.T, railSubID, initialAmount, planPayments string) (*fakeNMIPlanGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMIPlanGateway{railSubID: railSubID, planPayments: planPayments}
	f.amount.Store(initialAmount)
	f.updateMode.Store("ok")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/subscriptions/"):
			f.getCalls.Add(1)
			fmt.Fprintf(w, `{"object":"subscription","id":"%s","amount":"%s","delayed_condition":"active","next_billing_date":"2027-01-01","plan":{"object":"plan","id":"plan-x","plan_amount":"%s","plan_payments":"%s","day_frequency":"30"}}`,
				f.railSubID, f.amount.Load().(string), f.amount.Load().(string), f.planPayments)
		case r.Method == http.MethodPost:
			_ = r.ParseForm()
			if r.PostFormValue("recurring") != "update_subscription" {
				fmt.Fprint(w, "response=1")
				return
			}
			f.updateCalls.Add(1)
			f.updateForms = append(f.updateForms, map[string]string{
				"subscription_id": r.PostFormValue("subscription_id"),
				"plan_amount":     r.PostFormValue("plan_amount"),
				"plan_payments":   r.PostFormValue("plan_payments"),
			})
			switch f.updateMode.Load().(string) {
			case "rejected":
				fmt.Fprint(w, "response=3&responsetext=Invalid Subscription")
			case "stale":
				// Accepted on the wire, but the record never flips — the
				// read-back verify must catch this.
				fmt.Fprint(w, "response=1&responsetext=SUCCESS")
			default:
				f.amount.Store(r.PostFormValue("plan_amount"))
				fmt.Fprint(w, "response=1&responsetext=SUCCESS")
			}
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := nmi.NewClient("nmi", &config.NMIProviderSettings{
		SecurityKey: "test_security_key", WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.V5BaseURL = srv.URL
	client.QueryURL = srv.URL
	client.DirectPostURL = srv.URL
	return f, client
}

// fakeNMIClientSource arms the fake gateway's client for every subscription.
type fakeNMIClientSource struct{ client *nmi.NMIClient }

func (f fakeNMIClientSource) ResolveNMIClient(_ context.Context, _ uuid.UUID, _ *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return f.client, true, nil
}

// nmiNativeFixture: the plan-migration fixture re-armed with the REAL
// nmiPlanPusher over a fake gateway, and a pm-lookup that reports NO anchor
// (gateway-native detection must come from the pusher, not the instrument).
func nmiNativeFixture(t *testing.T, initialAmount, planPayments string) (*planMigrationFixture, *fakeNMIPlanGateway, uuid.UUID, string) {
	t.Helper()
	f := newPlanMigrationFixture(t)
	ctx := dbtest.WithTestMerchant(context.Background())
	subID, railSubID := f.createSubscriptionOnRail(t, ctx, f.lowPriceID, "nmi", false)
	gateway, client := newFakeNMIPlanGateway(t, railSubID, initialAmount, planPayments)
	f.pm = NewPlanMigrationService(f.repriceSvc, f.stripe, NewNMIPlanPusher(fakeNMIClientSource{client: client}),
		pmLookupFunc(func(_ context.Context, id uuid.UUID) (*models.PaymentMethod, error) {
			return &models.PaymentMethod{ID: id}, nil // no stored-credential anchor
		}))
	return f, gateway, subID, railSubID
}

// TestPlanMigration_NMINativeBoundaryPush: the full #815 mechanic — the push
// re-points the remote plan_amount (exact decimal, plan_payments preserved,
// schedule untouched), the internal cutover accompanies it, nothing is
// charged, and the rebill date never moves.
func TestPlanMigration_NMINativeBoundaryPush(t *testing.T) {
	f, gateway, subID, railSubID := nmiNativeFixture(t, "10.00", "7")
	ctx := dbtest.WithTestMerchant(context.Background())
	var periodEndBefore time.Time
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT current_period_ends_at FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&periodEndBefore))

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Matched)
	require.Equal(t, 1, res.Scheduled)
	require.Zero(t, res.Blocked)
	require.Equal(t, "applied_immediately", res.Outcomes[0].Disposition)
	require.Equal(t, 1, res.ByRail["nmi"].Auto)

	// Wire shape: one update, exact decimal amount, preserved plan_payments.
	require.EqualValues(t, 1, gateway.updateCalls.Load())
	form := gateway.updateForms[0]
	require.Equal(t, railSubID, form["subscription_id"])
	require.Equal(t, "9.00", form["plan_amount"], "9000000 micros must push as 9.00")
	require.Equal(t, "7", form["plan_payments"], "plan_payments must be preserved, not reset")
	require.Equal(t, "9.00", gateway.amount.Load().(string), "NMI now bills the target at the next rebill")

	// Internal cutover accompanied the push (provider truth flipped at push).
	priceID, productID := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.targetPriceID, priceID)
	require.Equal(t, f.targetProductID, productID)
	var specs string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT entitlements_spec_snapshot::text FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&specs))
	require.Contains(t, specs, "plan_b_access")

	// Money invariants: no charge, rebill date unchanged.
	var payments int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE subscription_id = $1`, subID).Scan(&payments))
	require.Zero(t, payments, "an NMI migration must never charge off-cycle")
	var periodEndAfter time.Time
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT current_period_ends_at FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&periodEndAfter))
	require.True(t, periodEndBefore.Equal(periodEndAfter), "the rebill date must never move")

	// Ledger: the row is applied.
	rows, err := f.repriceRepo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: res.BatchID}, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, models.RepriceStatusApplied, rows[0].Status)
	require.Equal(t, models.RepriceKindPlanChange, rows[0].Kind)
}

// TestPlanMigration_NMINativeFarFutureBlocksHonestly: NMI has no future-dated
// schedules — an effective date beyond the current period must NOT push (it
// would flip a whole rebill early); the row blocks and a re-run inside the
// final pre-effective period succeeds.
func TestPlanMigration_NMINativeFarFutureBlocksHonestly(t *testing.T) {
	f, gateway, subID, _ := nmiNativeFixture(t, "10.00", "0")
	ctx := dbtest.WithTestMerchant(context.Background())

	// Period ends +30d; effective +45d — beyond it.
	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID,
		EffectiveAt: f.clock.Now().Add(45 * 24 * time.Hour),
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	require.Contains(t, res.Outcomes[0].Reason, "nmi_deferred_push_required")
	require.Zero(t, gateway.updateCalls.Load(), "no early flip may be pushed")
	require.Equal(t, "10.00", gateway.amount.Load().(string))
	priceID, _ := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.lowPriceID, priceID, "blocked row leaves the sub untouched")

	// The sub renews into its final pre-effective period: re-run succeeds.
	_, err = f.pool.Exec(ctx, `UPDATE openrails.subscriptions SET current_period_ends_at = $1 WHERE id = $2`,
		f.clock.Now().Add(60*24*time.Hour), subID)
	require.NoError(t, err)
	res2, err := f.pm.Migrate(ctx, PlanMigrationRequest{
		SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID,
		EffectiveAt: f.clock.Now().Add(45 * 24 * time.Hour),
	})
	require.NoError(t, err)
	require.Equal(t, 1, res2.Scheduled)
	require.EqualValues(t, 1, gateway.updateCalls.Load())
	require.Equal(t, "9.00", gateway.amount.Load().(string))
}

// TestPlanMigration_NMINativePushFailureDegradesToBlocked: a gateway refusal
// leaves the sub on the old plan on BOTH sides, records the row blocked with
// the push error, and a re-run after the gateway heals migrates it.
func TestPlanMigration_NMINativePushFailureDegradesToBlocked(t *testing.T) {
	f, gateway, subID, _ := nmiNativeFixture(t, "10.00", "0")
	ctx := dbtest.WithTestMerchant(context.Background())
	gateway.updateMode.Store("rejected")

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	require.Contains(t, res.Outcomes[0].Reason, "rail_push_failed")
	require.Equal(t, "10.00", gateway.amount.Load().(string), "NMI still bills the old amount")
	priceID, _ := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.lowPriceID, priceID, "failed push leaves the sub untouched")
	rows, err := f.repriceRepo.List(ctx, SubscriptionRepriceFilter{RepriceBatchID: res.BatchID}, 10, 0)
	require.NoError(t, err)
	require.Equal(t, models.RepriceStatusBlocked, rows[0].Status)

	gateway.updateMode.Store("ok")
	res2, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res2.Scheduled)
	require.Equal(t, "9.00", gateway.amount.Load().(string))
	priceID, _ = f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.targetPriceID, priceID)
}

// TestPlanMigration_NMINativeGarbagePlanPaymentsBlocks: a NON-empty remote
// plan_payments we cannot parse must block the push — silently defaulting to
// 0 would rewrite a finite-payments schedule to bill-forever.
func TestPlanMigration_NMINativeGarbagePlanPaymentsBlocks(t *testing.T) {
	f, gateway, subID, _ := nmiNativeFixture(t, "10.00", "not-a-number")
	ctx := dbtest.WithTestMerchant(context.Background())

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	require.Contains(t, res.Outcomes[0].Reason, "unparseable plan_payments")
	require.Equal(t, int64(0), gateway.updateCalls.Load(), "no update may be attempted on an unreadable schedule")
	require.Equal(t, "10.00", gateway.amount.Load().(string))
	priceID, _ := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.lowPriceID, priceID)
}

// TestPlanMigration_NMINativeVerifyMismatchBlocks: an update the gateway
// accepts but never applies (ambiguous) must fail the read-back verify and
// land blocked — never an internal cutover NMI does not bill.
func TestPlanMigration_NMINativeVerifyMismatchBlocks(t *testing.T) {
	f, gateway, subID, _ := nmiNativeFixture(t, "10.00", "0")
	ctx := dbtest.WithTestMerchant(context.Background())
	gateway.updateMode.Store("stale")

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	require.Contains(t, res.Outcomes[0].Reason, "did not converge")
	priceID, _ := f.subscriptionRow(t, ctx, subID)
	require.Equal(t, f.lowPriceID, priceID, "no internal cutover without verified provider truth")
}

// TestPlanMigration_NMINativeIdempotentRerun: an applied NMI sub has LEFT the
// source-price cohort (price flipped at push), so a re-run is an empty-cohort
// no-op — nothing matched, nothing re-pushed.
func TestPlanMigration_NMINativeIdempotentRerun(t *testing.T) {
	f, gateway, _, _ := nmiNativeFixture(t, "10.00", "0")
	ctx := dbtest.WithTestMerchant(context.Background())

	res1, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Equal(t, 1, res1.Scheduled)
	require.EqualValues(t, 1, gateway.updateCalls.Load())

	res2, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: f.targetPriceID})
	require.NoError(t, err)
	require.Zero(t, res2.Matched, "the migrated sub is no longer on the source price")
	require.Zero(t, res2.Scheduled)
	require.Zero(t, res2.Blocked)
	require.EqualValues(t, 1, gateway.updateCalls.Load(), "re-run must not re-push")
}

// TestPlanMigration_NMINativeIntervalMismatchBlocks: an amount update cannot
// change the billing interval — a target on a different cycle blocks in BOTH
// preview and execute, with zero pushes.
func TestPlanMigration_NMINativeIntervalMismatchBlocks(t *testing.T) {
	f, gateway, _, _ := nmiNativeFixture(t, "10.00", "0")
	ctx := dbtest.WithTestMerchant(context.Background())

	// Annual target on the target product (8760h vs the source's 720h).
	annualTargetID := uuid.New()
	_, err := f.pool.Exec(ctx, `
		INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew, archived, key, created_at, updated_at)
		VALUES ($1,$2,$3,90000000,'USD',8760,true,false,$4,$5,$5)`,
		annualTargetID, f.targetProductID, f.merchantID, "planmig-annual-"+uuid.NewString()[:8], f.clock.Now())
	require.NoError(t, err)

	preview, err := f.pm.Preview(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: annualTargetID})
	require.NoError(t, err)
	require.Equal(t, 1, preview.Blocked)
	require.Equal(t, "nmi_interval_mismatch", preview.Outcomes[0].Reason)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: annualTargetID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	require.Equal(t, "nmi_interval_mismatch", res.Outcomes[0].Reason)
	require.Zero(t, gateway.updateCalls.Load(), "an inexpressible change must never be pushed")
}

// TestPlanMigration_NMINativeSubCentAmountBlocks: NMI bills whole cents — a
// micro-dollar target that is not cent-representable blocks with a reason.
func TestPlanMigration_NMINativeSubCentAmountBlocks(t *testing.T) {
	f, gateway, _, _ := nmiNativeFixture(t, "10.00", "0")
	ctx := dbtest.WithTestMerchant(context.Background())

	subCentID := uuid.New()
	_, err := f.pool.Exec(ctx, `
		INSERT INTO openrails.prices (id, product_id, merchant_id, amount, currency, access_duration_hours, auto_renew, archived, key, created_at, updated_at)
		VALUES ($1,$2,$3,9000001,'USD',720,true,false,$4,$5,$5)`,
		subCentID, f.targetProductID, f.merchantID, "planmig-subcent-"+uuid.NewString()[:8], f.clock.Now())
	require.NoError(t, err)

	res, err := f.pm.Migrate(ctx, PlanMigrationRequest{SourcePriceID: f.lowPriceID, TargetPriceID: subCentID})
	require.NoError(t, err)
	require.Equal(t, 1, res.Blocked)
	require.Equal(t, "nmi_amount_not_cent_representable", res.Outcomes[0].Reason)
	require.Zero(t, gateway.updateCalls.Load())
}
