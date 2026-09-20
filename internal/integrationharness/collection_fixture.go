//go:build integration

package integrationharness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/merchant"
)

// LoopbackNMISecurityKey is the security key seeded for loopback NMI accounts.
const LoopbackNMISecurityKey = "loopback-security-key"

// ArmLoopbackNMI arms one NMI account for the merchant in the canonical armed
// state (psps row + scoped secret); the runtime's sandbox gateway decides
// where its charges go.
func (h *Harness) ArmLoopbackNMI(rt *app.Runtime, mid merchant.ID) uuid.UUID {
	h.t.Helper()
	account := "loopback-" + strings.ReplaceAll(mid.String(), "-", "")[:12]
	SeedPSPs(h.ctx, h.t, rt, mid, config.PSPSet{"loopback": {Rail: models.RailNMI, AccountID: account, NMI: &config.NMIRailConfig{SecurityKey: LoopbackNMISecurityKey}}})
	var id uuid.UUID
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT id FROM openrails.psps WHERE merchant_id=$1 AND rail='nmi' AND account_id=$2`, mid.UUID(), account).Scan(&id))
	return id
}

// CollectionFixture is a past-due invoice on an NMI-vaulted instrument: the
// state a manual collection retry acts on.
type CollectionFixture struct {
	Merchant merchant.ID
	Customer uuid.UUID
	Method   uuid.UUID
	Invoice  uuid.UUID
	Vault    string
	Amount   int64
	Currency string
}

// SeedPastDueInvoice arms a loopback NMI account, vaults an instrument for a
// fresh arrears customer, issues an invoice for amount and marks it past due.
func (h *Harness) SeedPastDueInvoice(rt *app.Runtime, mid merchant.ID, currency string, amount int64) CollectionFixture {
	return h.SeedPastDueInvoiceForCustomer(rt, mid, uuid.New(), currency, amount)
}

// SeedPastDueInvoiceForCustomer binds the workflow to an independently verified
// identity, so customer-authenticated Client tests need no invented principal.
func (h *Harness) SeedPastDueInvoiceForCustomer(rt *app.Runtime, mid merchant.ID, customer uuid.UUID, currency string, amount int64) CollectionFixture {
	h.t.Helper()
	psp := h.ArmLoopbackNMI(rt, mid)
	method := uuid.New()
	vault := "vault-" + method.String()[:8]
	pool := h.sharedPool()
	_, err := pool.Exec(h.ctx, `INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2) ON CONFLICT DO NOTHING`, mid.UUID(), customer)
	require.NoError(h.t, err)
	_, err = gen.New(pool).CreatePaymentMethod(h.ctx, gen.CreatePaymentMethodParams{
		ID: method, MerchantID: mid.UUID(), CustomerID: customer, Rail: string(models.RailNMI), PspID: psp,
		InitialTransactionID: "init-" + method.String(), RailCustomerRef: vault,
	})
	require.NoError(h.t, err)
	dbtest.SeedNMIStoredCredentialRefs(h.ctx, h.t, pool, method)

	payer := identity.CustomerID(customer)
	mode := money.BillingModeArrears
	var invoice uuid.UUID
	require.NoError(h.t, rt.DB.RunInMerchantConn(merchant.WithID(h.ctx, mid), func(c context.Context) error {
		if _, err := rt.MoneyService.UpsertAccountSettings(c, payer, currency, money.AccountSettingsInput{BillingMode: &mode}); err != nil {
			return err
		}
		if _, err := rt.MoneyService.AccrueOwed(c, payer, currency, "workflow", uuid.NewString(), amount); err != nil {
			return err
		}
		issued, err := rt.MoneyService.FinalizeInvoice(c, payer, currency, time.Now().Add(-time.Hour), time.Now())
		if err != nil {
			return err
		}
		invoice = issued.ID
		_, err = rt.MoneyService.MarkInvoicesPastDue(c, time.Now().Add(31*24*time.Hour))
		return err
	}))
	return CollectionFixture{Merchant: mid, Customer: customer, Method: method, Invoice: invoice, Vault: vault, Amount: amount, Currency: currency}
}

// CollectionOperation is the durable invoice-collection operation attached to
// an invoice, read from the ledger.
type CollectionOperation struct {
	ID     uuid.UUID
	Status string
}

// LatestCollectionOperation returns the newest invoice_collection operation
// for the invoice.
func (h *Harness) LatestCollectionOperation(invoice uuid.UUID) CollectionOperation {
	h.t.Helper()
	var op CollectionOperation
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT id, status FROM openrails.rail_intents WHERE intent_type=$1 AND payload->>'invoice_id'=$2 ORDER BY created_at DESC LIMIT 1`,
		money.TypeInvoiceCollection, invoice.String()).Scan(&op.ID, &op.Status))
	return op
}

// OwedPaymentTransfers counts the settled owed_payment ledger transfers for
// the customer: the exactly-once money fact.
func (h *Harness) OwedPaymentTransfers(customer uuid.UUID) int {
	h.t.Helper()
	var n int
	require.NoError(h.t, h.sharedPool().QueryRow(h.ctx, `SELECT count(*) FROM openrails.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, customer).Scan(&n))
	return n
}

// MakeOperationDue collapses the verifier's first-look delay (intents.VerifyDelay)
// so a verify pass looks at the operation now.
func (h *Harness) MakeOperationDue(id uuid.UUID) {
	h.t.Helper()
	_, err := h.sharedPool().Exec(h.ctx, `UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1`, id)
	require.NoError(h.t, err)
}

// FireProviderIntentVerify inserts the periodic provider-intent verify job on
// the billing queue, the job the 5-minute schedule inserts, so whichever
// deployment runs workers on this database performs the pass now.
func (h *Harness) FireProviderIntentVerify(pool *pgxpool.Pool) {
	h.t.Helper()
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	require.NoError(h.t, err)
	_, err = client.Insert(h.ctx, riverjobs.ProviderIntentVerifyArgs{}, &river.InsertOpts{Queue: riverjobs.QueueBilling})
	require.NoError(h.t, err)
}

// ResolveOperation runs the operator resolution path, `openrails intents
// resolve`, as its own process against the deployment's database: the exact
// provider receipt (or provider-confirmed non-execution) an operator supplies
// after checking the gateway portal.
func (h *Harness) ResolveOperation(nmiGatewayURL string, mid merchant.ID, id uuid.UUID, receipt string, notExecuted bool) (string, error) {
	return h.ResolveOperationWith(config.ProviderSandboxConfig{NMIGatewayURL: nmiGatewayURL}, mid, id, "", receipt, notExecuted)
}

// ResolveOperationWith is ResolveOperation against any loopback provider
// set, naming the provider step of a multi-step operation when step is set.
func (h *Harness) ResolveOperationWith(sandbox config.ProviderSandboxConfig, mid merchant.ID, id uuid.UUID, step, receipt string, notExecuted bool) (string, error) {
	h.t.Helper()
	binary, err := h.openrailsBinary()
	require.NoError(h.t, err)
	dir := h.t.TempDir()
	redisAddr := ""
	if h.Redis != nil {
		redisAddr = h.Redis.Options().Addr
	}
	cfg := "env: dev\nmerchant_source: api\nsecret_backend: db\ntest_mode: sandbox\nprovider_write_mode: full\ndb:\n  url: " + h.DSN + "\nredis:\n  addr: " + redisAddr + "\n" + providerSandboxYAML(sandbox)
	require.NoError(h.t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600))
	args := []string{"intents", "resolve", "--config", filepath.Join(dir, "config.yaml"), "--merchant", "id:" + mid.String(), "--intent", id.String(), "--actor", "ops@example.test", "--reason", "gateway portal"}
	if step != "" {
		args = append(args, "--step", step)
	}
	if notExecuted {
		args = append(args, "--not-executed")
	} else {
		args = append(args, "--receipt", receipt)
	}
	cmd := exec.CommandContext(h.ctx, binary, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
