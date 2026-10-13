//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

func TestPSPReloadRejectsAccountReplacement(t *testing.T) {
	for _, source := range []string{"file", "vault"} {
		for _, change := range []string{"unused account", "another merchant's account", "rail", "environment"} {
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
				case "rail":
					doc.Value = before.PSPs["stripe"].Value
				case "environment":
					doc.Value.Environment = "live"
				}
				next.PSPs["nmi"] = doc
				if source == "file" {
					rt.MerchantConfig.Source().(*merchantdocs.FileSource).Put(mid, next)
				} else {
					w.vault.Put(t, "openrails/merchants/"+mid.UUID().String()+"/psps/nmi", doc.Value)
				}
				served, err := rt.MerchantConfig.Reload(ctx, mid)
				require.NoError(t, err)
				require.Contains(t, served.Rejected["psps/nmi"], "separate")
				require.Equal(t, before.PSPs["nmi"], served.PSPs["nmi"], "keep the account and its credentials")
				var retired bool
				require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT superseded_at IS NOT NULL FROM billing.psps WHERE id = $1`), w.psp["nmi"].UUID()).Scan(&retired))
				require.False(t, retired)
				if !other.IsZero() {
					owner, found, err := rt.Merchants.ResolvePSPByIdentity(ctx, "nmi", "sandbox", "replacement-account")
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, other, owner.MerchantID)
				}
				psp, err := w.client[remote].GetPSP(ctx, w.psp["nmi"])
				require.NoError(t, err)
				require.False(t, psp.Archived)
				end := e.periodEnd()
				e.toFreshPeriodEnd()
				w.runRenewals()
				require.True(t, e.periodEnd().After(end), "the original account still collects renewals")
			})
		}
	}
}

func TestPSPSeparateAccountKeepsExistingBilling(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	ctx := t.Context()
	e := enroll(t, w, "nmi", embedded)
	client := w.client[remote]
	original, err := client.GetPSP(ctx, w.psp["nmi"])
	require.NoError(t, err)

	_, err = client.CreatePSP(ctx, billing.CreatePSPParams{
		Key: "nmi", Rail: "nmi", Environment: "sandbox", AccountID: "replacement-account",
		Credentials: map[string]string{"security_key": "separate-account-key"},
	})
	require.Equal(t, "psp_key_taken", codeOf(err), "the API already refuses to replace an account")

	rotated, err := client.UpdatePSP(ctx, original.ID, billing.UpdatePSPParams{
		ExpectedRevision: &original.Revision,
		Credentials:      map[string]string{"security_key": "rotated-original-key"},
	})
	require.NoError(t, err)
	require.Equal(t, original.ID, rotated.ID)
	require.Equal(t, original.AccountID, rotated.AccountID)

	added, err := client.CreatePSP(ctx, billing.CreatePSPParams{
		Key: "separate", Rail: "nmi", Environment: "sandbox", AccountID: "replacement-account",
		Credentials: map[string]string{"security_key": "separate-account-key", "webhook_signing_secret": "separate-webhook"},
	})
	require.NoError(t, err)
	require.NotEqual(t, original.ID, added.ID)
	end := e.periodEnd()
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.True(t, e.periodEnd().After(end), "the subscription still renews on its original account")

	archived, err := client.UpdatePSP(ctx, original.ID, billing.UpdatePSPParams{Archived: true})
	require.NoError(t, err)
	require.True(t, archived.Archived)

	rt := engine.Graph(w.rt).Runtime
	scope, found, err := rt.Merchants.PSPScopeByID(ctx, client.MerchantID(), original.ID.UUID())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "rotated-original-key", scope.Secret("security_key"))
	active, err := rt.Merchants.ActivePSPScopesForRail(ctx, client.MerchantID(), "nmi")
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, added.ID.UUID(), active[0].ID)

	w.editDoc("psps/nmi", func(doc map[string]any) { doc["account_id"] = "third-account" })
	served, err := rt.MerchantConfig.Get(ctx, client.MerchantID())
	require.NoError(t, err)
	require.Contains(t, served.Rejected["psps/nmi"], "separate")
	require.Equal(t, original.AccountID, served.PSPs["nmi"].Value.AccountID)
	require.Equal(t, "rotated-original-key", served.PSPs["nmi"].Value.Secrets["security_key"])
	require.True(t, served.PSPs["nmi"].Value.Archived, "archived entries also retain their identity")
}
