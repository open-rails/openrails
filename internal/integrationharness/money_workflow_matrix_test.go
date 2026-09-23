//go:build integration

package integrationharness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// moneyDeployment is one deployment shape under test, restartable on the same
// database. The workflow program below only ever sees client().
type moneyDeployment struct {
	name     string
	merchant merchant.ID
	// runtime is the graph fixtures are seeded through (never the code under
	// test's own client path).
	runtime func() *app.Runtime
	client  func() *openrails.Client
	stop    func()
	start   func()
}

// moneyDeploymentBuilders builds, per subtest, one of the three shapes against
// one loopback NMI gateway: embedded (runtime object graph in this process),
// standalone (the actual cmd/openrails server as a separate OS process) and
// saas (the shared multi-merchant engine behind the hosted control plane).
// Exactly one deployment runs workers at a time, so a converged operation was
// converged by the deployment under test.
func moneyDeploymentBuilders(h *Harness, providers config.ProviderSandboxConfig) []struct {
	name  string
	build func(t *testing.T) moneyDeployment
} {
	ctx := context.Background()
	sandbox := func(cfg *config.Config) {
		loopback := providers
		cfg.ProviderSandbox = &loopback
	}
	return []struct {
		name  string
		build func(t *testing.T) moneyDeployment
	}{
		{"embedded", func(t *testing.T) moneyDeployment {
			var embedded *embed.Runtime
			start := func() {
				cfg := &config.Config{Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="},
					TestMode: config.CredentialPostureSandbox, MerchantConfigHTTP: true,
					SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN},
				}
				sandbox(cfg)
				rt, err := embed.New(ctx, embed.Options{Config: cfg, Redis: h.Redis, River: embed.RiverManagedByOpenRails(), RunWorkers: true})
				require.NoError(t, err)
				embedded = rt
			}
			stop := func() {
				if embedded == nil {
					return
				}
				require.NoError(t, embedded.Close(context.Background()))
				embedded = nil
			}
			dbtest.EnsureTestMerchant(ctx, t, h.Pool())
			start()
			t.Cleanup(stop)
			return moneyDeployment{
				name: "embedded", merchant: dbtest.TestMerchantID,
				runtime: func() *app.Runtime { return app.HostGraph(embedded).Runtime },
				client: func() *openrails.Client {
					client, err := embedded.Client(openrails.WithMerchantID(dbtest.TestMerchantID), openrails.WithCurrency("USD"), openrails.WithTimeout(30*time.Second))
					require.NoError(t, err)
					return client
				},
				stop: stop, start: start,
			}
		}},
		{"standalone", func(t *testing.T) moneyDeployment {
			// The in-process standalone graph seeds fixtures and mints the
			// API key; it runs no workers. The code under test is the process.
			standalone := h.StartStandalone("USD")
			process := h.StartStandaloneProcess(ProcessWithProviderSandbox(providers))
			t.Cleanup(process.Stop)
			return moneyDeployment{
				name: "standalone", merchant: dbtest.TestMerchantID,
				runtime: func() *app.Runtime { return standalone.App().Runtime },
				client: func() *openrails.Client {
					client, err := openrails.NewRemote(process.BaseURL, openrails.WithAPIKey(standalone.Token), openrails.WithMerchantID(dbtest.TestMerchantID), openrails.WithCurrency("USD"), openrails.WithTimeout(30*time.Second))
					require.NoError(t, err)
					return client
				},
				stop: process.Kill, start: process.Start,
			}
		}},
		{"saas", func(t *testing.T) moneyDeployment {
			hosted := h.StartHosted("USD", HostedWithWorkers(), HostedWithConfig(sandbox))
			t.Cleanup(hosted.Stop)
			owner := hosted.RegisterUser("owner")
			tenant := hosted.ProvisionMerchant(owner, "money-"+uuid.NewString()[:8])
			return moneyDeployment{
				name: "saas", merchant: tenant.ID,
				runtime: hosted.AppRuntime,
				client:  func() *openrails.Client { return tenant.Client() },
				stop:    hosted.Stop, start: hosted.Start,
			}
		}},
	}
}

// TestBillingProviderRecoveryAcrossDeployments runs one billing lifecycle per
// deployment. Each graph serves invoice collection and both providers' tier
// changes; the explicit stop/start recovery boundaries inside each scenario stay.
// This removes nine cold graph constructions while keeping every provider case
// on embedded, standalone and hosted transports.
func TestBillingProviderRecoveryAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	nmiGateway := NewFakeNMIGateway(t)
	stripeGateway := NewFakeStripeGateway(t)
	providers := config.ProviderSandboxConfig{NMIGatewayURL: nmiGateway.URL, StripeAPIURL: stripeGateway.URL}
	h.Pool()
	_, err := h.openrailsBinary()
	require.NoError(t, err)
	parent := t
	for _, builder := range moneyDeploymentBuilders(h, providers) {
		t.Run(builder.name, func(t *testing.T) {
			h.SetT(t)
			t.Cleanup(func() { h.SetT(parent) })
			deployment := builder.build(t)
			t.Log("invoice uncertainty and operator resolution")
			checkProviderUncertaintyResolvedThroughOperator(t, h, nmiGateway, deployment)
			t.Log("invoice recovery after deployment restart")
			checkRestartConvergesCollectionExactlyOnce(t, h, nmiGateway, deployment)
			t.Log("Stripe tier change, replay, restart and refusal")
			checkStripeTierChangeReplay(t, h, stripeGateway, deployment)
			t.Log("NMI tier change, replay, restart and refusal")
			checkNMITierChangeReplay(t, h, nmiGateway, deployment)
		})
	}
}

func requireRefusal(t *testing.T, err error, sentinel error, code string) {
	t.Helper()
	require.ErrorIs(t, err, sentinel)
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, code, status.Code)
}

// checkProviderUncertaintyResolvedThroughOperator: a collection charge whose
// response is lost stays one unknown operation under one provider identity —
// the same key replays it, a new key is refused, nothing is resent — and
// settles exactly once only when an operator supplies the exact provider
// receipt through `openrails intents resolve`; a wrong receipt, a receipt the
// provider does not show and a contradicted non-execution are refused. The
// provider is a loopback NMI fake; no live gateway behavior is proven.
func checkProviderUncertaintyResolvedThroughOperator(t *testing.T, h *Harness, gateway *FakeNMIGateway, d moneyDeployment) {
	t.Helper()
	ctx := context.Background()
	gateway.SetMode(NMISaleUncertain)
	gateway.SetVisible(false)
	sales := gateway.SaleCount()
	fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 5_000_000)
	client := d.client()
	retry := openrails.InvoiceCollectionRetryRequest{InvoiceID: fixture.Invoice, IdempotencyKey: "lost-" + uuid.NewString()[:8], PaymentMethodID: openrails.PaymentMethodID(fixture.Method)}

	result, err := client.RetryInvoiceCollection(ctx, retry)
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.Equal(t, "attempted", result.Attempt.Status, "an unresolved operation answers with its live attempt")
	require.Equal(t, "past_due", result.Invoice.Status)
	require.Equal(t, sales+1, gateway.SaleCount())
	op := h.LatestCollectionOperation(fixture.Invoice)
	require.Equal(t, "unknown_needs_verify", op.Status)
	sale, ok := gateway.SaleForOrder(op.ID.String())
	require.True(t, ok, "the wire order id is the operation id")
	require.Equal(t, "5.00", sale.Amount)
	require.Equal(t, fixture.Vault, sale.Vault)

	replayed, err := client.RetryInvoiceCollection(ctx, retry)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, result.Attempt.ID, replayed.Attempt.ID)
	require.Equal(t, "attempted", replayed.Attempt.Status)
	fresh := retry
	fresh.IdempotencyKey = "second-" + uuid.NewString()[:8]
	_, err = client.RetryInvoiceCollection(ctx, fresh)
	requireRefusal(t, err, openrails.ErrConflict, openrails.CodeInvoiceRetryOutcomeUnknown)
	require.Equal(t, sales+1, gateway.SaleCount(), "nothing is resent while the outcome is unknown")

	// Operator resolution: the provider portal shows nothing yet, so
	// the receipt cannot be confirmed; a foreign receipt never can.
	const rejected = "provider evidence does not establish this operation's outcome"
	out, err := h.ResolveOperation(gateway.URL, d.merchant, op.ID, sale.TransactionID, false)
	require.Error(t, err, "receipt the provider does not show: %s", out)
	require.Contains(t, out, rejected)
	out, err = h.ResolveOperation(gateway.URL, d.merchant, op.ID, "tx-unrelated", false)
	require.Error(t, err, "unrelated receipt: %s", out)
	require.Contains(t, out, rejected)
	require.Equal(t, "unknown_needs_verify", h.LatestCollectionOperation(fixture.Invoice).Status)

	gateway.SetVisible(true)
	out, err = h.ResolveOperation(gateway.URL, d.merchant, op.ID, "", true)
	require.Error(t, err, "non-execution while the provider shows the sale: %s", out)
	require.Contains(t, out, rejected)
	require.Contains(t, out, "provider shows successful sale "+sale.TransactionID)
	out, err = h.ResolveOperation(gateway.URL, d.merchant, op.ID, sale.TransactionID, false)
	require.NoError(t, err, out)
	require.Contains(t, out, `"status": "succeeded"`)

	invoice, err := client.GetMerchantInvoice(ctx, fixture.Invoice)
	require.NoError(t, err)
	require.Equal(t, "paid", invoice.Status)
	require.Zero(t, invoice.AmountDue)
	require.Equal(t, fixture.Amount, invoice.AmountPaid)
	attempts, total, err := client.ListInvoicePaymentAttempts(ctx, fixture.Invoice, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, "settled", attempts[0].Status)
	require.Equal(t, sale.TransactionID, *attempts[0].RailPaymentID)
	settled, err := client.RetryInvoiceCollection(ctx, retry)
	require.NoError(t, err)
	require.True(t, settled.Replayed)
	require.Equal(t, "settled", settled.Attempt.Status)
	_, err = client.RetryInvoiceCollection(ctx, fresh)
	requireRefusal(t, err, openrails.ErrConflict, openrails.CodeInvoiceNotRetryable)
	require.Equal(t, sales+1, gateway.SaleCount(), "resolution never charges")
	require.Equal(t, 1, h.OwedPaymentTransfers(fixture.Customer))
}

// checkRestartConvergesCollectionExactlyOnce: the deployment is stopped after
// the collection charge crossed the submission fence and before its receipt
// (the gateway answered with a communication error), the receipt becomes
// visible while it is down, and the deployment started again from the same
// database refuses to resend, verifies the operation with its own worker and
// settles the invoice once. Embedded and SaaS rebuild the runtime graph;
// standalone kills and relaunches the cmd/openrails process. The verifier's
// first-look delay and five-minute cadence are collapsed by making the
// operation due and inserting the scheduled verify job; the pass itself runs
// inside the restarted deployment.
func checkRestartConvergesCollectionExactlyOnce(t *testing.T, h *Harness, gateway *FakeNMIGateway, d moneyDeployment) {
	t.Helper()
	ctx := context.Background()
	gateway.SetMode(NMISaleUncertain)
	gateway.SetVisible(false)
	sales := gateway.SaleCount()
	fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 7_500_000)
	retry := openrails.InvoiceCollectionRetryRequest{InvoiceID: fixture.Invoice, IdempotencyKey: "restart-" + uuid.NewString()[:8], PaymentMethodID: openrails.PaymentMethodID(fixture.Method)}
	result, err := d.client().RetryInvoiceCollection(ctx, retry)
	require.NoError(t, err)
	require.Equal(t, "attempted", result.Attempt.Status)
	require.Equal(t, sales+1, gateway.SaleCount())
	op := h.LatestCollectionOperation(fixture.Invoice)
	require.Equal(t, "unknown_needs_verify", op.Status)
	sale, ok := gateway.SaleForOrder(op.ID.String())
	require.True(t, ok)

	d.stop()
	gateway.SetVisible(true)
	d.start()

	client := d.client()
	fresh := retry
	fresh.IdempotencyKey = "after-restart-" + uuid.NewString()[:8]
	_, err = client.RetryInvoiceCollection(ctx, fresh)
	requireRefusal(t, err, openrails.ErrConflict, openrails.CodeInvoiceRetryOutcomeUnknown)
	replayed, err := client.RetryInvoiceCollection(ctx, retry)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, "attempted", replayed.Attempt.Status)
	require.Equal(t, sales+1, gateway.SaleCount(), "the restarted deployment does not resend")

	h.MakeOperationDue(op.ID)
	h.FireProviderIntentVerify(h.Pool(), op.ID)
	require.Eventually(t, func() bool {
		invoice, err := client.GetMerchantInvoice(ctx, fixture.Invoice)
		return err == nil && invoice.Status == "paid"
	}, 90*time.Second, 500*time.Millisecond, "the restarted deployment's verifier settles from the visible receipt")

	invoice, err := client.GetMerchantInvoice(ctx, fixture.Invoice)
	require.NoError(t, err)
	require.Zero(t, invoice.AmountDue)
	require.Equal(t, fixture.Amount, invoice.AmountPaid)
	attempts, total, err := client.ListInvoicePaymentAttempts(ctx, fixture.Invoice, 10, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, "settled", attempts[0].Status)
	require.Equal(t, sale.TransactionID, *attempts[0].RailPaymentID)
	settled, err := client.RetryInvoiceCollection(ctx, retry)
	require.NoError(t, err)
	require.True(t, settled.Replayed)
	require.Equal(t, "settled", settled.Attempt.Status)
	require.Equal(t, "succeeded", h.LatestCollectionOperation(fixture.Invoice).Status)
	require.Equal(t, sales+1, gateway.SaleCount())
	require.Equal(t, 1, h.OwedPaymentTransfers(fixture.Customer))
	require.True(t, strings.HasPrefix(sale.TransactionID, "tx-"))
}
