//go:build integration

package custodymigration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodymigration"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #657 x or#297: a payment-source update and a custody remap must never
// straddle each other. The remap refuses an instrument named by an unresolved
// swap (matched on the intent's FROZEN method ids, since the subscription's
// link only moves at finalize), and the swap's executor refuses under the
// instrument's row lock once a remap has re-attributed it.

// seedSwapIntent enqueues a due nmi_payment_source_update moving subID from
// oldMethod onto newMethod, both vaulted at the fixture's old PSP.
func seedSwapIntent(t *testing.T, fx *custodyFixture, subID, oldMethod, newMethod uuid.UUID, status string) uuid.UUID {
	t.Helper()
	oldVault, newVault := fx.method(t, oldMethod).RailCustomerRef, fx.method(t, newMethod).RailCustomerRef
	row, err := intents.NewStore(fx.db).Enqueue(fx.ctx, intents.EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(), Provider: "nmi", PspID: fx.oldPSP.ID,
		IntentType: intents.TypeNMIPaymentSourceUpdate, SubscriptionID: &subID,
		Payload: intents.NMIPaymentSourceUpdatePayload{
			NewPaymentMethodID: newMethod, NewRailCustomerRef: newVault, NewPspID: fx.oldPSP.ID,
			OldPaymentMethodID: &oldMethod, OldRailCustomerRef: oldVault,
		},
		IdempotencyKey: intents.NMIPaymentSourceUpdateIdempotencyKey(subID, newVault, 0),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute),
		Origin:         intents.OriginUser, OriginReason: "or297 x #657 test",
	})
	require.NoError(t, err)
	if status != intents.StatusPending {
		_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE openrails.rail_intents SET status = $2 WHERE id = $1`, row.ID, status)
		require.NoError(t, err)
	}
	return row.ID
}

func intentRow(t *testing.T, fx *custodyFixture, id uuid.UUID) (status, code, reason string) {
	t.Helper()
	var c, r *string
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		`SELECT status, result_evidence->>'code', last_failure_reason FROM openrails.rail_intents WHERE id = $1`, id).Scan(&status, &c, &r))
	if c != nil {
		code = *c
	}
	if r != nil {
		reason = *r
	}
	return status, code, reason
}

// The remap is refused while a swap naming the instrument — as its NEW side,
// which the subscription does not yet reference — is unresolved in any state,
// and proceeds once the swap is resolved.
func TestCustodyMigration_RefusesUnresolvedPaymentSourceUpdate(t *testing.T) {
	for _, status := range []string{intents.StatusPending, intents.StatusInFlight, intents.StatusFailedRetryable, intents.StatusUnknownNeedsVerify} {
		t.Run(status, func(t *testing.T) {
			fx := newCustodyFixture(t)
			oldVault, newVault := "vault-"+uuid.NewString()[:8], "vault-"+uuid.NewString()[:8]
			oldMethod, subID := fx.seedPSPVaultedCard(t, oldVault)
			newMethod := fx.seedStandaloneCard(t, subID, newVault)
			intentID := seedSwapIntent(t, fx, subID, oldMethod, newMethod, status)

			exp := fx.export(custodymigration.ImportedToken{SourceRailCustomerRef: newVault, Token: "tok_" + uuid.NewString()[:12]})
			plan, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, false))
			require.NoError(t, err)
			require.Equal(t, custodymigration.OutcomeBlocked, plan.Rows[0].Outcome)
			require.Equal(t, custodymigration.ReasonPaymentSourceUpdateUnresolved, plan.Rows[0].Reason)

			res, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
			require.NoError(t, err)
			require.Equal(t, 1, res.Counts[custodymigration.OutcomeBlocked])
			require.Equal(t, fx.oldPSP.ID, fx.method(t, newMethod).PspID, "a refused instrument keeps its PSP")
			requireMigrationCount(t, fx, newMethod, 0)

			_, err = fx.db.Pool().Exec(fx.ctx,
				`UPDATE openrails.rail_intents SET status = 'succeeded', executed_at = now() WHERE id = $1`, intentID)
			require.NoError(t, err)
			res, err = custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
			require.NoError(t, err)
			require.Equal(t, 1, res.Counts[custodymigration.OutcomeRemapped])
			require.Equal(t, fx.newPSP.ID, fx.method(t, newMethod).PspID)
		})
	}
}

// The OLD side is pinned too: the verifier anchors on its vault handle until
// the swap resolves. failed_retryable is the state the or#297 charge
// predicate does not cover (it already refuses in_flight/unknown_needs_verify
// through the subscription's link).
func TestCustodyMigration_RefusesUnresolvedPaymentSourceUpdateOnOldSide(t *testing.T) {
	fx := newCustodyFixture(t)
	oldVault, newVault := "vault-"+uuid.NewString()[:8], "vault-"+uuid.NewString()[:8]
	oldMethod, subID := fx.seedPSPVaultedCard(t, oldVault)
	newMethod := fx.seedStandaloneCard(t, subID, newVault)
	seedSwapIntent(t, fx, subID, oldMethod, newMethod, intents.StatusFailedRetryable)

	exp := fx.export(custodymigration.ImportedToken{SourceRailCustomerRef: oldVault, Token: "tok_" + uuid.NewString()[:12]})
	res, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
	require.NoError(t, err)
	require.Equal(t, custodymigration.OutcomeBlocked, res.Rows[0].Outcome)
	require.Equal(t, custodymigration.ReasonPaymentSourceUpdateUnresolved, res.Rows[0].Reason)
	requireMigrationCount(t, fx, oldMethod, 0)
}

// Real remap vs real executor, started together on a due swap intent whose
// target the manifest re-attributes. Exactly one of them wins: a remapped
// instrument implies a terminal psp_mismatch swap with no provider write and
// the subscription still on its old method; a completed swap implies the
// remap was refused and the instrument kept its PSP. Never both.
func TestCustodyMigration_RemapVersusPaymentSourceUpdateRace(t *testing.T) {
	var remapped, refused int
	for i := 0; i < 6; i++ {
		fx := newCustodyFixture(t)
		oldVault, newVault := "vault-"+uuid.NewString()[:8], "vault-"+uuid.NewString()[:8]
		oldMethod, subID := fx.seedPSPVaultedCard(t, oldVault)
		newMethod := fx.seedStandaloneCard(t, subID, newVault)
		gateway, client := newFakeSwapGateway(t, "railsub-"+oldVault, oldVault)
		runner := &intents.Runner{
			Store:    intents.NewStore(fx.db),
			Registry: intents.NewRegistry(intents.NewNMIPaymentSourceUpdateHandler(fx.db, staticResolver{client}, nil)),
			Config:   &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
		}
		intentID := seedSwapIntent(t, fx, subID, oldMethod, newMethod, intents.StatusPending)
		exp := fx.export(custodymigration.ImportedToken{SourceRailCustomerRef: newVault, Token: "tok_" + uuid.NewString()[:12]})

		var (
			wg         sync.WaitGroup
			res        custodymigration.Result
			rerr, xerr error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 1 {
				time.Sleep(time.Duration(i) * time.Millisecond)
			}
			res, rerr = custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
		}()
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				time.Sleep(time.Duration(i) * time.Millisecond)
			}
			_, xerr = runner.RunExecuteOnce(fx.ctx)
		}()
		close(start)
		wg.Wait()
		require.NoError(t, rerr)
		require.NoError(t, xerr)

		status, code, reason := intentRow(t, fx, intentID)
		var subMethod uuid.UUID
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT payment_method_id FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&subMethod))
		switch res.Rows[0].Outcome {
		case custodymigration.OutcomeRemapped:
			remapped++
			require.Equal(t, intents.StatusFailedTerminal, status, "iteration %d: %s", i, reason)
			require.Equal(t, intents.EvidenceCodePSPMismatch, code, "iteration %d: %s", i, reason)
			require.Zero(t, gateway.updateCalls.Load(), "iteration %d: a re-attributed target never reaches the provider", i)
			require.Equal(t, oldMethod, subMethod, "iteration %d", i)
			require.Equal(t, fx.newPSP.ID, fx.method(t, newMethod).PspID)
		case custodymigration.OutcomeBlocked:
			refused++
			// Between finalize and MarkSucceeded the subscription already
			// references the target while the intent is still in_flight, so
			// the or#297 charge predicate may be the one that fires.
			require.Contains(t, []string{custodymigration.ReasonPaymentSourceUpdateUnresolved, custodymigration.ReasonChargeInFlight}, res.Rows[0].Reason, "iteration %d", i)
			require.Equal(t, intents.StatusSucceeded, status, "iteration %d: %s", i, reason)
			require.EqualValues(t, 1, gateway.updateCalls.Load(), "iteration %d", i)
			require.Equal(t, newMethod, subMethod, "iteration %d", i)
			require.Equal(t, fx.oldPSP.ID, fx.method(t, newMethod).PspID, "iteration %d: a refused instrument keeps its PSP", i)
		default:
			t.Fatalf("iteration %d: unexpected remap outcome %s (%s)", i, res.Rows[0].Outcome, res.Rows[0].Reason)
		}
	}
	t.Logf("remap won %d, swap won %d", remapped, refused)
}

// The other arm, deterministic: the remap has already flipped the target when
// the swap arrives. The producer refuses it (re-read under the row lock, no
// intent row, no provider call); a producer that had frozen the swap before
// the flip is refused by the executor with psp_mismatch and no provider call.
func TestCustodyMigration_RemapThenPaymentSourceUpdateIsRefused(t *testing.T) {
	fx := newCustodyFixture(t)
	oldVault, newVault := "vault-"+uuid.NewString()[:8], "vault-"+uuid.NewString()[:8]
	oldMethod, subID := fx.seedPSPVaultedCard(t, oldVault)
	newMethod := fx.seedStandaloneCard(t, subID, newVault)
	gateway, client := newFakeSwapGateway(t, "railsub-"+oldVault, oldVault)
	runner := &intents.Runner{
		Store:    intents.NewStore(fx.db),
		Registry: intents.NewRegistry(intents.NewNMIPaymentSourceUpdateHandler(fx.db, staticResolver{client}, nil)),
		Config:   &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
	}

	exp := fx.export(custodymigration.ImportedToken{SourceRailCustomerRef: newVault, Token: "tok_" + uuid.NewString()[:12]})
	res, err := custodymigration.Migrate(fx.ctx, fx.opts(exp, true))
	require.NoError(t, err)
	require.Equal(t, 1, res.Counts[custodymigration.OutcomeRemapped])
	require.Equal(t, fx.newPSP.ID, fx.method(t, newMethod).PspID)

	sub, err := subscriptions.NewSubscriptionRepo(fx.db).GetByID(fx.ctx, subID)
	require.NoError(t, err)
	target, err := paymentmethods.NewPaymentMethodRepo(fx.db).GetByID(fx.ctx, newMethod)
	require.NoError(t, err)
	through := &intents.PaymentSourceUpdateThrough{Runner: runner, DB: fx.db}
	_, err = through.ExecutePaymentSourceUpdate(fx.ctx, sub, target, intents.OriginUser, "post-remap swap")
	require.ErrorIs(t, err, subscriptions.ErrPaymentMethodProviderAccountMismatch)
	require.Zero(t, gateway.updateCalls.Load())
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM openrails.rail_intents WHERE subscription_id = $1`, subID).Scan(&n))
	require.Zero(t, n, "a refused producer leaves no intent")

	intentID := seedSwapIntent(t, fx, subID, oldMethod, newMethod, intents.StatusPending)
	_, err = runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	status, code, reason := intentRow(t, fx, intentID)
	require.Equal(t, intents.StatusFailedTerminal, status, reason)
	require.Equal(t, intents.EvidenceCodePSPMismatch, code)
	require.Zero(t, gateway.updateCalls.Load())
	var subMethod uuid.UUID
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT payment_method_id FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&subMethod))
	require.Equal(t, oldMethod, subMethod)
}

// ---------------------------------------------------------------- helpers

// seedStandaloneCard mints a second PSP-vaulted instrument for subID's
// customer — the swap target; no subscription rides it yet.
func (fx *custodyFixture) seedStandaloneCard(t *testing.T, subID uuid.UUID, vaultID string) uuid.UUID {
	t.Helper()
	methodID := uuid.New()
	var customerID uuid.UUID
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT customer_id FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&customerID))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, fx.db.RunInMerchantConn(fx.ctx, func(ctx context.Context) error {
		_, err := fx.db.Gen(ctx).CreatePaymentMethod(ctx, gen.CreatePaymentMethodParams{
			ID: methodID, MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: customerID,
			Rail: "nmi", RailCustomerRef: vaultID, PspID: fx.oldPSP.ID, Custodian: "psp",
			CreatedAt: now, UpdatedAt: now,
		})
		return err
	}))
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = fx.db.Pool().Exec(bg, `DELETE FROM openrails.custody_migrations WHERE payment_method_id = $1`, methodID)
		_, _ = fx.db.Pool().Exec(bg, `DELETE FROM openrails.payment_methods WHERE id = $1`, methodID)
	})
	return methodID
}

type staticResolver struct{ client *nmi.NMIClient }

func (r staticResolver) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return r.client, true, nil
}

// fakeSwapGateway answers the recurring-record read and the
// update_subscription write the swap intent makes.
type fakeSwapGateway struct {
	vault       atomic.Value
	updateCalls atomic.Int64
}

func newFakeSwapGateway(t *testing.T, railSubID, initialVault string) (*fakeSwapGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeSwapGateway{}
	f.vault.Store(initialVault)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/subscriptions/"):
			fmt.Fprintf(w, `{"object":"subscription","id":"%s","customer_vault_id":"%s","delayed_condition":"active"}`, railSubID, f.vault.Load().(string))
		case r.Method == http.MethodPost:
			_ = r.ParseForm()
			if r.PostFormValue("recurring") == "update_subscription" {
				f.updateCalls.Add(1)
				f.vault.Store(r.PostFormValue("customer_vault_id"))
			}
			fmt.Fprint(w, "response=1&responsetext=SUCCESS")
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := nmi.NewClient("nmi", &config.NMIProviderSettings{SecurityKey: "test_security_key", WebhookSecret: "test_secret"}, true)
	require.NoError(t, err)
	client.V5BaseURL, client.QueryURL, client.DirectPostURL = srv.URL, srv.URL, srv.URL
	return f, client
}
