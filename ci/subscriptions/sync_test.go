//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
)

// A PSP is its account, and an account's records stay with it whatever its
// label names next, from a file or from Vault. A label moved to an unused
// account registers that account as a new PSP; a label naming an account the
// merchant cannot hold (another merchant's, one another label of the
// merchant already declares, or real money beside a non-live deployment) is
// refused, and the account it named keeps serving and renewing.
func TestPSPLabelMoveLeavesAccountRecordsInPlace(t *testing.T) {
	for _, source := range []string{"file", "vault"} {
		for _, change := range []string{"unused account", "another merchant's account", "another label's account", "real money"} {
			t.Run(source+"/"+change, func(t *testing.T) {
				t.Parallel()
				var w *world
				if source == "vault" {
					w = newVaultWorld(t)
				} else {
					w = newWorld(t)
				}
				e := enroll(t, w, "nmi", embedded)
				rt := engine.Graph(w.rt).Runtime
				ctx := t.Context()
				mid := w.client[embedded].MerchantID()
				original := w.psp["nmi"].UUID()
				before, err := rt.MerchantConfig.Get(ctx, mid)
				require.NoError(t, err)
				next := before.Clone()
				doc := next.PSPs["nmi"]
				var other billing.MerchantID
				switch change {
				case "unused account", "another merchant's account":
					doc.Value.AccountID = "replacement-account"
					if change == "another merchant's account" {
						other = billing.MerchantID(uuid.New())
						_, _, err := rt.Merchants.RegisterForRestore(ctx, other, "other-"+uuid.NewString()[:8])
						require.NoError(t, err)
						foreign, err := rt.Merchants.SyncConfig(ctx, other, merchantdocs.Set{}, merchantdocs.Set{
							PSPs: map[string]merchantdocs.Doc[merchantdocs.PSP]{"other": doc},
						})
						require.NoError(t, err)
						require.Empty(t, foreign.Rejected)
					}
				case "another label's account":
					doc.Value = before.PSPs["stripe"].Value
				case "real money":
					doc.Value.Environment = "live"
				}
				next.PSPs["nmi"] = doc
				charges := len(w.nmi.ledger(""))
				if source == "file" {
					rt.MerchantConfig.Source().(*merchantdocs.FileSource).Put(mid, next)
				} else {
					w.vault.Put(t, "openrails/merchants/"+mid.UUID().String()+"/psps/nmi", doc.Value)
				}
				served, err := rt.MerchantConfig.Reload(ctx, mid)
				require.NoError(t, err)
				require.Len(t, w.nmi.ledger(""), charges, "a reload moves no money")

				// The account's records stay with it.
				sub := w.subscription(embedded, e.sub)
				require.Equal(t, original, sub.PSPID.UUID())
				method, err := billing.ParsePaymentMethodID(e.method)
				require.NoError(t, err)
				var methodPSP uuid.UUID
				require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT psp_id FROM billing.payment_methods WHERE id = $1`), method.UUID()).Scan(&methodPSP))
				require.Equal(t, original, methodPSP)
				var live int
				require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.psps WHERE environment = 'live'`)).Scan(&live))
				require.Zero(t, live, "no real-money PSP joins a deployment of test seams")

				var superseded bool
				require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT superseded_at IS NOT NULL FROM billing.psps WHERE id = $1`), original).Scan(&superseded))
				if change == "unused account" {
					require.Equal(t, "replacement-account", served.PSPs["nmi"].Value.AccountID, "the label names the new account")
					require.True(t, superseded, "the label moved; the account it named is no longer declared")
					replacement := merchants.PspID("nmi", "sandbox", "replacement-account")
					var key string
					require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT key FROM billing.psps WHERE id = $1 AND superseded_at IS NULL`), replacement).Scan(&key))
					require.Equal(t, "nmi", key)
					return
				}
				require.NotEmpty(t, served.Rejected["psps/nmi"], "the label cannot name that account")
				require.Equal(t, before.PSPs["nmi"], served.PSPs["nmi"], "the account it named keeps serving")
				require.False(t, superseded)
				if change == "another label's account" {
					require.Equal(t, before.PSPs["stripe"], served.PSPs["stripe"], "the label already holding the account keeps it")
					require.Empty(t, served.Rejected["psps/stripe"])
				}
				if !other.IsZero() {
					owner, found, err := rt.Merchants.ResolvePSPByIdentity(ctx, "nmi", "sandbox", "replacement-account")
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, other, owner.MerchantID)
				}
				end := e.periodEnd()
				e.toFreshPeriodEnd()
				w.runRenewals()
				require.True(t, e.periodEnd().After(end), "the account still collects renewals")
			})
		}
	}
}

// Adding a second account beside one with billing, rotating the first's
// credentials or archiving it never moves its records: its subscription
// renews on it, and its archived credentials stay readable while it drains.
func TestPSPAccountsKeepTheirBilling(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	ctx := t.Context()
	e := enroll(t, w, "nmi", embedded)
	client := w.client[remote]
	original, err := client.GetPSP(ctx, w.psp["nmi"])
	require.NoError(t, err)

	rotated, err := client.UpdatePSP(ctx, original.ID, billing.UpdatePSPParams{
		ExpectedRevision: &original.Revision,
		Credentials:      map[string]string{"security_key": "rotated-original-key"},
	})
	require.NoError(t, err)
	require.Equal(t, original.ID, rotated.ID)
	require.Equal(t, original.AccountID, rotated.AccountID)

	added, err := client.CreatePSP(ctx, billing.CreatePSPParams{
		Key: "second", Rail: "nmi", Environment: "sandbox", AccountID: "second-account",
		Credentials: map[string]string{"security_key": "second-account-key", "webhook_signing_secret": "second-webhook"},
	})
	require.NoError(t, err)
	require.NotEqual(t, original.ID, added.ID)
	end := e.periodEnd()
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.True(t, e.periodEnd().After(end), "the subscription renews on its own account")
	require.Equal(t, original.ID, w.subscription(embedded, e.sub).PSPID)

	archived, err := client.UpdatePSP(ctx, original.ID, billing.UpdatePSPParams{Archived: true})
	require.NoError(t, err)
	require.True(t, archived.Archived)
	rt := engine.Graph(w.rt).Runtime
	scope, found, err := rt.Merchants.PSPScopeByID(ctx, client.MerchantID(), original.ID.UUID())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "rotated-original-key", scope.Secret("security_key"), "an archived account's credentials stay readable while it drains")
	active, err := rt.Merchants.ActivePSPScopesForRail(ctx, client.MerchantID(), "nmi")
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, added.ID.UUID(), active[0].ID)
	require.Equal(t, original.ID, w.subscription(embedded, e.sub).PSPID, "archiving moves no record")
}
