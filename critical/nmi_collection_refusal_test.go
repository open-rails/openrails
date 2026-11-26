//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func TestNMICollectionRefusal(t *testing.T) {
	t.Run("OR-J26-S55/a-never-sent-collection-retains-its-proof", func(t *testing.T) {
		for _, interrupted := range []bool{false, true} {
			name := "ordinary-completion"
			if interrupted {
				name = "interrupted-completion"
			}
			t.Run(name, func(t *testing.T) {
				a, p, customer := usageBillingWorld(t, false)
				psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
				require.NoError(t, err)
				require.Len(t, psps.Items, 1)
				psp := psps.Items[0]
				client, err := nmi.NewAccountClient(slog.Default(), a.alpha.MerchantID().UUID(), psp.ID.UUID(), psp.GatewayID, &config.NMIProviderSettings{SecurityKey: "critical-nmi-key", Environment: config.EnvironmentTest})
				require.NoError(t, err)
				key := client.PostureKey()
				restore := func() {
					providerposture.Process().Verify(t.Context(), key, func(context.Context) (providerposture.Verdict, error) { return providerposture.Simulated, nil })
				}
				t.Cleanup(restore)
				status := providerposture.Process().Verify(t.Context(), key, func(context.Context) (providerposture.Verdict, error) { return providerposture.Live, nil })
				require.False(t, status.Armed(), "a real-money verdict cannot arm a test account")
				if interrupted {
					_, err := a.pool.Exec(t.Context(), "CREATE FUNCTION "+pgx.Identifier{a.schema, "refuse_collection_finish"}.Sanitize()+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.intent_type='order_collection' AND NEW.status='failed_terminal' THEN RAISE EXCEPTION 'critical collection completion fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER refuse_collection_finish BEFORE UPDATE ON "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" FOR EACH ROW EXECUTE FUNCTION "+pgx.Identifier{a.schema, "refuse_collection_finish"}.Sanitize()+"()")
					require.NoError(t, err)
					t.Cleanup(func() {
						_, err := a.pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS refuse_collection_finish ON "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize())
						require.NoError(t, err)
					})
				}
				oneUsage(t, a.alpha, usageInput(customer, "unsent-collection", 3))
				a.clock.Advance(time.Second)
				fleet := startFleet(t, a.world)
				runUsageDue(t, fleet)
				var state string
				var evidence []byte
				// A refused collection leaves the invoice due at once, so each
				// usage scan that finds it open queues another. Every scan has
				// run once the fleet's start-up pass ends, and the invoice is
				// open again once none of their collections is running.
				operations := pgx.Identifier{a.schema, "provider_intents"}.Sanitize()
				require.Eventually(t, func() bool {
					passes, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.billing_pass").States(rivertype.JobStateCompleted, rivertype.JobStateDiscarded).First(1))
					if err != nil || len(passes.Jobs) == 0 {
						return false
					}
					var running int
					err = a.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM "+operations+" WHERE merchant_id=$1 AND intent_type=$2 AND status IN ('pending','in_flight')),status,result_evidence FROM "+operations+" WHERE merchant_id=$1 AND intent_type=$2 ORDER BY created_at,id LIMIT 1", a.alpha.MerchantID().UUID(), intents.TypeInvoiceCollection).Scan(&running, &state, &evidence)
					return err == nil && running == 0
				}, 20*time.Second, 20*time.Millisecond, "the registered worker must finish its first collection")
				p.money(t, 0, 0)
				var proof struct {
					NotExecuted bool   `json:"not_executed"`
					Code        string `json:"not_executed_code"`
				}
				require.NoError(t, json.Unmarshal(evidence, &proof))
				if interrupted {
					require.Equal(t, intents.StatusUnknownNeedsVerify, state)
					var retained struct {
						SubmittedAt string          `json:"submitted_at"`
						Qualified   json.RawMessage `json:"qualified_collection_nonexecution"`
					}
					require.NoError(t, json.Unmarshal(evidence, &retained))
					require.NotEmpty(t, retained.SubmittedAt)
					require.NotEmpty(t, retained.Qualified, "the qualified refusal must survive a failed local completion")
					_, err := a.pool.Exec(t.Context(), "DROP TRIGGER refuse_collection_finish ON "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize())
					require.NoError(t, err)
					a.clock.Advance(time.Hour)
					require.Eventually(t, func() bool {
						wakeJobs(t, fleet, "openrails.provider_operation")
						err := a.pool.QueryRow(t.Context(), "SELECT status,result_evidence FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" WHERE merchant_id=$1 AND intent_type=$2 ORDER BY created_at,id LIMIT 1", a.alpha.MerchantID().UUID(), intents.TypeInvoiceCollection).Scan(&state, &evidence)
						return err == nil && state == intents.StatusFailedTerminal
					}, 15*time.Second, 20*time.Millisecond)
					p.money(t, 0, 0)
					require.NoError(t, json.Unmarshal(evidence, &proof))
				}
				require.Equal(t, intents.StatusFailedTerminal, state, "a proven unsent charge must not wait for a nonexistent provider receipt")
				require.True(t, proof.NotExecuted)
				require.Equal(t, "not_dispatched", proof.Code)
				invoice := oneUsageInvoice(t, a, customer)
				require.Equal(t, billing.InvoiceOpen, invoice.Status)
				require.Zero(t, invoice.Usage.AmountPaid)
				require.EqualValues(t, 30_000_000, invoice.Usage.AmountDue)
				restore()
				runUsageDue(t, fleet)
				require.Eventually(t, func() bool { return oneUsageInvoice(t, a, customer).Status == billing.InvoiceComplete }, 15*time.Second, 20*time.Millisecond)
				runUsageDue(t, fleet)
				p.money(t, 1, 3000)
			})
		}
	})
}

// executeEngineRenewal admits and executes the due obligation through its normal runner.
func executeEngineRenewal(t *testing.T, a *authorityWorld, sub *billing.Subscription) gen.BillingProviderIntent {
	t.Helper()
	a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
	rt := embedded.Graph(a.engine).Runtime
	var row gen.BillingProviderIntent
	require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
		var err error
		row, err = rt.MoneyService.AdmitDueSubscriptionCollection(ctx, sub.ID.UUID(), a.clock.Now())
		if err != nil {
			return err
		}
		row, err = rt.IntentRunner().ExecuteNowByID(ctx, row.ID)
		return err
	}))
	return row
}

func TestNMIEngineRenewalRecovery(t *testing.T) {
	t.Run("OR-J14-S08/gateway-refusals-settle-through-dunning", func(t *testing.T) {
		for _, code := range []int{300, 410, 411, 440, 441, 460, 461} {
			t.Run(fmt.Sprint(code), func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				p.core.SetDecline("4242", fmt.Sprint(code))
				reference := ""
				calls := 0
				p.core.Intercept(nmiSaleRequest, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
					calls++
					if code == 410 {
						response := serve()
						require.NoError(t, response.Body.Close())
						reference = p.core.LastDecline().TransactionID
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf("response=3&response_code=%d&transactionid=%s&responsetext=Invalid+Customer+Vault+Id", code, reference)))}, nil
				})
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusFailedTerminal, row.Status)
				require.NoError(t, intents.ValidateSubscriptionCollectionTerminal(row))
				refusal, found, err := intents.LoadRecurringDecline(row)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, code, refusal.ResponseCode)
				require.Equal(t, reference, refusal.Reference)
				require.Empty(t, refusal.Issuer.IssuerCode)
				state := "past_due"
				if code == 461 {
					state = "awaiting_method"
				}
				current := subscriptionState(t, a, sub.ID, state)
				require.NotNil(t, current.Dunning)
				if code != 461 {
					require.NotNil(t, current.Dunning.NextRetryAt)
				}
				findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.submission.unresolved"})
				require.NoError(t, err)
				require.Empty(t, findings.Items)
				require.Equal(t, 1, calls)
				p.money(t, 1, 1000)
				if code != 410 {
					return
				}
				accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
				require.NoError(t, err)
				p.core.SetDecline("4242", "")
				p.core.ClearIntercepts()
				advanceTo(a.world, *current.Dunning.NextRetryAt)
				rt := embedded.Graph(a.engine).Runtime
				require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
					var err error
					row, err = rt.MoneyService.AdmitDueSubscriptionCollection(ctx, sub.ID.UUID(), a.clock.Now())
					if err != nil {
						return err
					}
					row, err = rt.IntentRunner().ExecuteNowByID(ctx, row.ID)
					return err
				}))
				require.Equal(t, intents.StatusSucceeded, row.Status)
				next, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
				require.NoError(t, err)
				require.Equal(t, 1, next.Attempt)
				require.Equal(t, accepted.OrderReference, next.OrderReference)
				require.Len(t, p.core.Attempts(), 3)
				p.money(t, 2, 2000)
			})
		}
	})
	t.Run("OR-J14-S09/duplicate-and-ambiguous-renewals-stay-unknown", func(t *testing.T) {
		for _, code := range []string{"duplicate", "duplicate-report", "duplicate-older-report", "420", "421", "430", "transport"} {
			t.Run(code, func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				if code == "duplicate" {
					p.core.RefuseDuplicates(1)
				} else if strings.HasPrefix(code, "duplicate-") {
					p.core.SetDecline("4242", "300")
					p.core.Intercept(nmiSaleRequest, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
						response := serve()
						require.NoError(t, response.Body.Close())
						response.Body = io.NopCloser(strings.NewReader("response=3&response_code=300&responsetext=Duplicate+transaction+REFID:3187654321"))
						return response, nil
					})
					p.core.Intercept(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/query.php") }, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
						response := serve()
						body, err := io.ReadAll(response.Body)
						require.NoError(t, err)
						require.NoError(t, response.Body.Close())
						report := strings.ReplaceAll(string(body), "<response_text>DECLINE</response_text>", "<response_text>Duplicate transaction REFID:3187654321</response_text>")
						if code == "duplicate-older-report" {
							report = strings.ReplaceAll(report, "<order_description>", "<order_description>older-")
						}
						response.Body = io.NopCloser(strings.NewReader(report))
						return response, nil
					})
				} else if code == "transport" {
					p.core.LoseSales(1)
				} else {
					p.core.Intercept(nmiSaleRequest, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("response=3&response_code=" + code))}, nil
					})
				}
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				rt := embedded.Graph(a.engine).Runtime
				a.clock.Advance(25 * time.Hour)
				require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
					if strings.HasPrefix(code, "duplicate-") {
						attempts, err := intents.ReadNMIOrderAttempts(ctx, row, rt.CollectionResolver)
						require.NoError(t, err)
						require.Equal(t, 1, attempts.Transactions, "an older duplicate still owns a possible charge")
						require.False(t, attempts.Declined)
					}
					var err error
					row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
					return err
				}))
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				require.Nil(t, current.Dunning)
				p.money(t, 1, 1000)
				require.LessOrEqual(t, len(p.core.Attempts()), 2, "enrollment and at most one renewal")
			})
		}
	})
	t.Run("OR-J14-S10/operator-qualifies-the-exact-renewal-receipt", func(t *testing.T) {
		a, p, sub := nmiEngineMembership(t)
		p.core.HideSales(1)
		row := executeEngineRenewal(t, a, sub)
		require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
		rt := embedded.Graph(a.engine).Runtime
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		a.clock.Advance(6 * time.Minute)
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var err error
			row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
			return err
		}))
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.submission.unresolved"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Contains(t, *findings.Items[0].RecommendedAction, "openrails intents resolve --intent "+row.ID.String())
		candidate, found, err := intents.LoadCollectionCandidate(row)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, p.core.LastSale().TransactionID, candidate.TransactionID)
		before := row.ResultEvidence
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			_, err := rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Order search is empty"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected)
			require.ErrorContains(t, err, "--receipt")
			for _, reference := range []string{"unknown-transaction", p.core.Sales()[0].TransactionID} {
				_, err := rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{ProviderReference: reference, Actor: "operator", Reason: "Checked receipt"})
				require.ErrorIs(t, err, intents.ErrResolutionRejected)
			}
			_, err = rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{Step: "charge", ProviderReference: p.core.LastSale().TransactionID, Actor: "operator", Reason: "Checked receipt"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected)
			current, err := intents.NewStore(rt.DB).Get(ctx, row.ID)
			require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
			require.JSONEq(t, string(before), string(current.ResultEvidence))
			return err
		}))
		p.core.Reveal()
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			_, err := rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Checked order"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected)
			receipt, found, err := intents.ReadNMICollectionReceipt(ctx, row, rt.CollectionResolver, p.core.LastSale().TransactionID)
			require.NoError(t, err)
			require.True(t, found)
			_, err = intents.NewStore(rt.DB).RetainCollectedReceipt(ctx, row, receipt)
			require.NoError(t, err)
			_, err = rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Checked order"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected)
			row, err = rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{ProviderReference: p.core.LastSale().TransactionID, Actor: "operator", Reason: "Checked receipt"})
			return err
		}))
		require.Equal(t, intents.StatusSucceeded, row.Status)
		require.NoError(t, intents.ValidateSubscriptionCollectionTerminal(row))
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, *sub.CurrentPeriodEndsAt, *current.CurrentPeriodStartsAt)
		findings, err = a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.submission.unresolved", Status: "fixed"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Contains(t, string(row.ResultEvidence), `"operator_resolution"`)
		p.money(t, 2, 2000)
	})
	t.Run("OR-J14-S11/operator-releases-empty-renewal-order-for-next-attempt", func(t *testing.T) {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprint(late), func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				p.core.LoseSales(1)
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
				require.NoError(t, err)
				rt := embedded.Graph(a.engine).Runtime
				ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
				resolution := intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Checked empty NMI order and attest release"}
				p.core.QueryUnavailable(true)
				require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					_, err := rt.IntentRunner().Resolve(ctx, row.ID, resolution)
					require.ErrorIs(t, err, intents.ErrResolutionRejected)
					return nil
				}))
				p.core.QueryUnavailable(false)
				require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					err := rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
						outcome := intents.TerminalWithEvidence("unproven", map[string]any{"not_executed": true, "not_executed_code": "not_executed", "declined": false})
						return intents.NewStore(rt.DB.NewWithPgxTx(tx)).CompleteSubscriptionCollection(ctx, row, outcome, a.clock.Now())
					})
					require.ErrorContains(t, err, "no qualified nonexecution proof")
					return nil
				}))
				require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					var err error
					row, err = rt.IntentRunner().Resolve(ctx, row.ID, resolution)
					return err
				}))
				require.Equal(t, intents.StatusFailedTerminal, row.Status)
				require.NoError(t, intents.ValidateSubscriptionCollectionTerminal(row))
				unattested := row
				var terminal map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(row.ResultEvidence, &terminal))
				delete(terminal, "operator_resolution")
				unattested.ResultEvidence, err = json.Marshal(terminal)
				require.NoError(t, err)
				require.Error(t, intents.ValidateSubscriptionCollectionTerminal(unattested))
				var evidence struct {
					NotExecuted bool `json:"not_executed"`
					Operator    struct {
						Actor       string `json:"actor"`
						Reason      string `json:"reason"`
						NotExecuted bool   `json:"not_executed"`
					} `json:"operator_resolution"`
				}
				require.NoError(t, json.Unmarshal(row.ResultEvidence, &evidence))
				require.True(t, evidence.NotExecuted)
				require.True(t, evidence.Operator.NotExecuted)
				require.Equal(t, resolution.Actor, evidence.Operator.Actor)
				require.Equal(t, resolution.Reason, evidence.Operator.Reason)
				invoice, err := a.alpha.GetInvoice(t.Context(), billing.InvoiceID(accepted.InvoiceID))
				require.NoError(t, err)
				require.Equal(t, billing.InvoiceOpen, invoice.Status)

				if late {
					p.core.AddSale(nmimock.Sale{Vault: accepted.Instrument.RailCustomerRef, OrderID: accepted.OrderReference, Amount: "10.00", At: a.clock.Now()})
					require.NoError(t, p.persist(t.Context()))
				}
				fleet := hostFleet(t, a.world)
				fleet.PeriodicJobs().Clear()
				require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
				require.NoError(t, fleet.Start(t.Context()))
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				require.Eventually(t, func() bool {
					current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
					return err == nil && current.CurrentPeriodStartsAt.Equal(*sub.CurrentPeriodEndsAt)
				}, 20*time.Second, 20*time.Millisecond)
				var payload []byte
				require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT payload FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" WHERE subscription_id=$1 AND intent_type='subscription_collection' AND id<>$2", sub.ID.UUID(), row.ID).Scan(&payload))
				var next subscriptions.SubscriptionCollectionPayload
				require.NoError(t, json.Unmarshal(payload, &next))
				require.Equal(t, 1, next.Attempt)
				require.Equal(t, accepted.OrderReference, next.OrderReference)
				p.money(t, 2, 2000)
				attempts := 2
				if late {
					attempts = 1
				}
				require.Len(t, p.core.Attempts(), attempts, "a late charge must be recovered before another send")
				archive := archiveBytes(t, a.alpha)
				target, restored := archiveTarget(t, a.world)
				_, err = restored.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
				require.NoError(t, err)
				var restoredEvidence []byte
				require.NoError(t, target.pool.QueryRow(t.Context(), "SELECT result_evidence FROM "+pgx.Identifier{target.schema, "provider_intents"}.Sanitize()+" WHERE id=$1", row.ID).Scan(&restoredEvidence))
				require.JSONEq(t, string(row.ResultEvidence), string(restoredEvidence))
			})
		}
	})
	t.Run("OR-J14-S12/operator-retains-the-orders-definitive-decline", func(t *testing.T) {
		for _, code := range []int{202, 410} {
			t.Run(fmt.Sprint(code), func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				p.core.SetDecline("4242", fmt.Sprint(code))
				p.core.Intercept(nmiSaleRequest, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
					response := serve()
					response.StatusCode = http.StatusBadGateway
					p.core.QueryUnavailable(true)
					return response, nil
				})
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				p.core.QueryUnavailable(false)
				rt := embedded.Graph(a.engine).Runtime
				require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
					p.core.EditSale(p.core.LastDecline().TransactionID, func(s *nmimock.Sale) { s.Condition = "pending" })
					_, err := rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Unsettled order"})
					require.ErrorIs(t, err, intents.ErrResolutionRejected)
					p.core.EditSale(p.core.LastDecline().TransactionID, func(s *nmimock.Sale) { s.Condition = "" })
					row, err = rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Confirmed NMI decline"})
					return err
				}))
				require.Equal(t, intents.StatusFailedTerminal, row.Status)
				require.NoError(t, intents.ValidateSubscriptionCollectionTerminal(row))
				refusal, found, err := intents.LoadRecurringDecline(row)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, code, refusal.ResponseCode)
				require.Contains(t, string(row.ResultEvidence), `"operator_resolution"`)
				current := subscriptionState(t, a, sub.ID, "past_due")
				require.NotNil(t, current.Dunning.NextRetryAt)
				p.money(t, 1, 1000)
			})
		}
	})

	t.Run("OR-J14-S13/stripe-engine-renewals-reject-operator-resolution", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		p.unavailableEvidence(true)
		row := executeEngineRenewal(t, a, sub)
		require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
		before := row.ResultEvidence
		rt := embedded.Graph(a.engine).Runtime
		require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
			for _, resolution := range []intents.Resolution{
				{ProviderReference: "pi_unknown", Actor: "operator", Reason: "Checked receipt"},
				{NotExecuted: true, Actor: "operator", Reason: "Checked order"},
			} {
				_, err := rt.IntentRunner().Resolve(ctx, row.ID, resolution)
				require.ErrorIs(t, err, intents.ErrResolutionRejected)
			}
			current, err := intents.NewStore(rt.DB).Get(ctx, row.ID)
			require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
			require.JSONEq(t, string(before), string(current.ResultEvidence))
			return err
		}))
		require.Len(t, p.effects(t), 1)
	})
}

func TestSubmittedRenewalGrace(t *testing.T) {
	t.Run("OR-J04-S96/keep-policy-verification-does-not-lock-the-membership", func(t *testing.T) {
		a, p, sub := nmiEngineMembership(t)
		p.core.LoseSales(1)
		row := executeEngineRenewal(t, a, sub)
		require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
		a.clock.Advance(intents.LostSubmissionSettle)
		lock, err := a.pool.Begin(t.Context())
		require.NoError(t, err)
		defer lock.Rollback(context.Background())
		_, err = lock.Exec(t.Context(), "SELECT id FROM "+pgx.Identifier{a.schema, "customers"}.Sanitize()+" WHERE id=$1 FOR UPDATE", sub.CustomerID.UUID())
		require.NoError(t, err)
		_, err = lock.Exec(t.Context(), "SELECT id FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id=$1 FOR UPDATE", sub.ID.UUID())
		require.NoError(t, err)
		rt := embedded.Graph(a.engine).Runtime
		done := make(chan error, 1)
		go func() {
			done <- rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
				var err error
				row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
				return err
			})
		}()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.NoError(t, lock.Rollback(t.Context()))
			require.NoError(t, <-done)
			t.Fatal("keep-policy verification waited for customer or subscription locks")
		}
		require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
		require.Empty(t, intents.EvidenceString(row, "unresolved_renewal_grace_at"))
		p.money(t, 1, 1000)
	})
	t.Run("OR-J04-S94/an-unanswered-dunning-retry-keeps-access-suspended", func(t *testing.T) {
		a, p, sub := nmiEngineMembership(t, func(w *world) {
			suspendHeld(w)
			m := cfgMerchant(w.cfg, "alpha")
			m.MerchantSettings.DunningPolicy.AccessDuringDunning = billing.DunningAccessSuspend
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		p.core.SetDecline("4242", "202")
		row := executeEngineRenewal(t, a, sub)
		require.Equal(t, intents.StatusFailedTerminal, row.Status)
		current := subscriptionState(t, a, sub.ID, "past_due")
		require.False(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"))
		p.core.SetDecline("4242", "")
		p.core.LoseSales(1)
		advanceTo(a.world, *current.Dunning.NextRetryAt)
		rt := embedded.Graph(a.engine).Runtime
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var err error
			row, err = rt.MoneyService.AdmitDueSubscriptionCollection(ctx, sub.ID.UUID(), a.clock.Now())
			if err != nil {
				return err
			}
			row, err = rt.IntentRunner().ExecuteNowByID(ctx, row.ID)
			return err
		}))
		require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
		a.clock.Advance(intents.LostSubmissionSettle)
		require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var err error
			row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
			return err
		}))
		require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
		subscriptionState(t, a, sub.ID, "past_due")
		require.False(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"))
		p.money(t, 1, 1000)
	})
	t.Run("OR-J04-S95/an-unresolved-renewal-finding-survives-a-grace-write-failure", func(t *testing.T) {
		a, p, sub := nmiEngineMembership(t, suspendHeld)
		p.core.LoseSales(1)
		row := executeEngineRenewal(t, a, sub)
		require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
		_, err := a.pool.Exec(t.Context(), "CREATE FUNCTION "+pgx.Identifier{a.schema, "refuse_grace"}.Sanitize()+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.result_evidence ? 'unresolved_renewal_grace_at' THEN RAISE EXCEPTION 'critical grace write fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER refuse_grace BEFORE UPDATE ON "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" FOR EACH ROW EXECUTE FUNCTION "+pgx.Identifier{a.schema, "refuse_grace"}.Sanitize()+"()")
		require.NoError(t, err)
		a.clock.Advance(intents.LostSubmissionSettle)
		rt := embedded.Graph(a.engine).Runtime
		require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
			var err error
			row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
			return err
		}))
		require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
		require.Empty(t, intents.EvidenceString(row, "unresolved_renewal_grace_at"))
		require.NotNil(t, row.LastFailureReason)
		require.Contains(t, *row.LastFailureReason, "critical grace write fault")
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.submission.unresolved"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Equal(t, row.ID.String(), findings.Items[0].SubjectKey)
		p.money(t, 1, 1000)
	})
	t.Run("OR-J04-S93/a-successful-renewal-keeps-its-access-write-count", func(t *testing.T) {
		a, _, sub := nmiEngineMembership(t, suspendHeld)
		counts := func() (int, int) {
			var g, w int
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM "+pgx.Identifier{a.schema, "grants"}.Sanitize()+" WHERE customer_id=$1), (SELECT count(*) FROM "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" WHERE customer_id=$1)", sub.CustomerID.UUID()).Scan(&g, &w))
			return g, w
		}
		beforeGrants, beforeWindows := counts()
		row := executeEngineRenewal(t, a, sub)
		require.Equal(t, intents.StatusSucceeded, row.Status)
		afterGrants, afterWindows := counts()
		require.Equal(t, 3, afterGrants-beforeGrants)
		require.Equal(t, 2, afterWindows-beforeWindows)
		t.Logf("normal renewal: grants %d -> %d; product_access %d -> %d", beforeGrants, afterGrants, beforeWindows, afterWindows)
	})
	t.Run("OR-J04-S90/a-sent-renewal-keeps-access-until-its-answer", func(t *testing.T) {
		for _, late := range []bool{false, true} {
			for _, answer := range []string{"paid", "declined-keep", "declined-suspend"} {
				declined := answer != "paid"
				suspendDunning := answer == "declined-suspend"
				t.Run(fmt.Sprintf("late=%t/%s", late, answer), func(t *testing.T) {
					a, p, sub := nmiEngineMembership(t, func(w *world) {
						suspendHeld(w)
						if suspendDunning {
							m := cfgMerchant(w.cfg, "alpha")
							m.MerchantSettings.DunningPolicy.AccessDuringDunning = billing.DunningAccessSuspend
							setCfgMerchant(&w.cfg, "alpha", m)
						}
					})
					rt := embedded.Graph(a.engine).Runtime
					ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
					var row gen.BillingProviderIntent
					if late {
						p.core.QueryUnavailable(true)
						row = executeEngineRenewal(t, a, sub)
						require.Empty(t, intents.EvidenceString(row, "submitted_at"))
						a.clock.Advance(17 * time.Hour)
						require.False(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"))
						p.core.QueryUnavailable(false)
						require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
							var err error
							row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
							return err
						}))
						require.Equal(t, intents.StatusFailedRetryable, row.Status)
					}
					if declined {
						p.core.SetDecline("4242", "202")
					}
					p.core.Intercept(nmiSaleRequest, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
						response := serve()
						response.StatusCode = http.StatusBadGateway
						p.core.QueryUnavailable(true)
						return response, nil
					})
					if late {
						require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
							var err error
							row, err = rt.IntentRunner().ExecuteNowByID(ctx, row.ID)
							return err
						}))
					} else {
						row = executeEngineRenewal(t, a, sub)
					}
					require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
					require.NotEmpty(t, intents.EvidenceString(row, "submitted_at"))
					// The first verification past the settle delay keeps access open.
					a.clock.Advance(intents.LostSubmissionSettle)
					require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
						var err error
						row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
						return err
					}))
					require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
					require.True(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"), "an unanswered sent charge restores access")
					advanceTo(a.world, sub.CurrentPeriodEndsAt.Add(18*time.Hour))
					require.True(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"), "a sent charge outlasts the suspend allowance")
					var grants, windows int
					counts := func() (int, int) {
						var g, w int
						require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM "+pgx.Identifier{a.schema, "grants"}.Sanitize()+" WHERE customer_id=$1), (SELECT count(*) FROM "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" WHERE customer_id=$1)", sub.CustomerID.UUID()).Scan(&g, &w))
						return g, w
					}
					grants, windows = counts()
					for range 2 {
						require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
							var err error
							row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
							return err
						}))
						require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
					}
					g, w := counts()
					require.Equal(t, grants, g, "verification adds no grants")
					require.Equal(t, windows, w, "verification adds no access windows")
					p.core.QueryUnavailable(false)
					require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
						var err error
						row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
						return err
					}))
					if declined {
						require.Equal(t, intents.StatusFailedTerminal, row.Status)
						subscriptionState(t, a, sub.ID, "past_due")
					} else {
						require.Equal(t, intents.StatusSucceeded, row.Status)
						current := subscriptionState(t, a, sub.ID, "active")
						require.Equal(t, *sub.CurrentPeriodEndsAt, *current.CurrentPeriodStartsAt)
					}
					require.Equal(t, !suspendDunning, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"), "the answer applies payment or dunning access rules")
				})
			}
		}
	})
	t.Run("OR-J04-S91/releasing-a-sent-renewal-restores-the-suspend-allowance", func(t *testing.T) {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprint(late), func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t, suspendHeld)
				p.core.LoseSales(1)
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				if late {
					a.clock.Advance(18 * time.Hour)
				} else {
					a.clock.Advance(6 * time.Minute)
				}
				rt := embedded.Graph(a.engine).Runtime
				ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
				require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					var err error
					row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
					return err
				}))
				require.True(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"))
				setControl(t, a.world, "rebills_held", true, a.alpha.MerchantID())
				require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					var err error
					row, err = rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Confirmed empty order"})
					return err
				}))
				require.Equal(t, intents.StatusFailedTerminal, row.Status)
				require.Equal(t, !late, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"))
				advanceTo(a.world, sub.CurrentPeriodEndsAt.Add(19*time.Hour))
				require.False(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"))
				subscriptionState(t, a, sub.ID, "active")
			})
		}
	})
	t.Run("OR-J04-S92/an-unsent-renewal-still-suspends-at-its-allowance", func(t *testing.T) {
		for _, held := range []bool{false, true} {
			t.Run(fmt.Sprint(held), func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t, suspendHeld)
				if held {
					setControl(t, a.world, "rebills_held", true, a.alpha.MerchantID())
					advanceTo(a.world, *sub.CurrentPeriodEndsAt)
				} else {
					p.core.QueryUnavailable(true)
					row := executeEngineRenewal(t, a, sub)
					require.Empty(t, intents.EvidenceString(row, "submitted_at"))
				}
				advanceTo(a.world, sub.CurrentPeriodEndsAt.Add(16*time.Hour+47*time.Minute))
				require.True(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"))
				a.clock.Advance(2 * time.Minute)
				require.False(t, entitled(t, a.alpha, sub.CustomerID, "nmi:engine"))
				p.money(t, 1, 1000)
			})
		}
	})
}
