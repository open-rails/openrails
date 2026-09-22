//go:build integration

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// This drives the real application transaction with the complete consumer
// artifact. Only provider transports are controlled: every reference still goes
// through the production strict read-only term/identity validation helpers.
func TestCatalogApplicationWholeDoujinsArtifactReferencePreflight(t *testing.T) {
	for _, failure := range []string{"", "wrong-terms", "missing-plan", "account-changed"} {
		t.Run(failure, func(t *testing.T) {
			s, ctx := applicationService(t)
			mid, err := merchant.Require(ctx)
			require.NoError(t, err)
			owner := dbtest.SharedSuperuserPGXPool(t)
			nmiAccountID := "mobius-" + mid.String()
			for _, account := range []struct{ key, rail, id string }{{"mobius", "nmi", nmiAccountID}, {"ccbill", "ccbill", "ccbill-" + mid.String()}, {"solana", "solana", "solana-" + mid.String()}} {
				_, err = owner.Exec(ctx, "INSERT INTO billing.psps(id,merchant_id,key,rail,environment,account_id) VALUES($1,$2,$3,$4,'live',$5)", uuid.New(), mid.UUID(), account.key, account.rail, account.id)
				require.NoError(t, err)
			}
			raw, err := os.ReadFile("testdata/doujins-catalog-application.yaml")
			require.NoError(t, err)
			application, err := openrails.ParseCatalogApplicationYAML(raw)
			require.NoError(t, err)
			providerReads := 0
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method, "application must never create an external plan")
				providerReads++
				amount := "23.00"
				if r.URL.Path == "/plans/premium" {
					amount = "19.00"
				}
				if failure == "wrong-terms" {
					amount = "999.00"
				}
				if r.URL.Path != "/plans/premium" && r.URL.Path != "/plans/premium_new" {
					t.Errorf("unexpected provider read %s", r.URL.Path)
				}
				fmt.Fprintf(w, `{"object":"plan","id":%q,"plan_amount":%q,"day_frequency":"30","plan_payments":"0"}`, strings.TrimPrefix(r.URL.Path, "/plans/"), amount)
			}))
			t.Cleanup(gateway.Close)
			nmi := newMobiusAdapterWithServer(t, gateway.URL)
			nmi.svc.rt.RailConfigs = railresolve.FixedSet{"mobius": {Rail: models.RailNMI, AccountID: nmiAccountID, NMI: &config.NMIRailConfig{SecurityKey: "test-security-key"}}}
			_, plan, chain, submitter, address := catalogReferencePlanFixture(t)
			if failure == "missing-plan" {
				delete(chain.data, address)
			}
			verifications := 0
			verify := func(ctx context.Context, key, rail, accountID, productKey string, req CreatePriceRequest, link map[string]string) (map[string]string, error) {
				verifications++
				// A different DB session must acquire this lock NOWAIT while a provider
				// read is in progress. No merchant transaction may span this callback.
				var unlocked uuid.UUID
				if err := owner.QueryRow(ctx, "SELECT id FROM billing.merchants WHERE id=$1 FOR UPDATE NOWAIT", mid.UUID()).Scan(&unlocked); err != nil {
					return nil, err
				}
				if failure == "account-changed" && verifications == 1 {
					if _, err := owner.Exec(ctx, "UPDATE billing.psps SET archived=true WHERE merchant_id=$1 AND key='mobius'", mid.UUID()); err != nil {
						return nil, err
					}
				}
				switch rail {
				case "nmi":
					return verifyNMICatalogReference(ctx, nmi, accountID, req, link)
				case "solana":
					return verifySolanaCatalogReference(ctx, plan, chain, "USDC", productKey, req, link)
				case "ccbill":
					return declaredCCBillCatalogReference(link)
				default:
					return nil, fmt.Errorf("unexpected provider %s/%s", key, rail)
				}
			}
			receipt, err := s.applyCatalog(ctx, *application, verify)
			if failure != "" {
				require.Error(t, err)
				var products, receipts int
				require.NoError(t, owner.QueryRow(ctx, "SELECT count(*) FROM billing.products WHERE merchant_id=$1", mid.UUID()).Scan(&products))
				require.NoError(t, owner.QueryRow(ctx, "SELECT count(*) FROM billing.catalog_applications WHERE merchant_id=$1", mid.UUID()).Scan(&receipts))
				require.Zero(t, products)
				require.Zero(t, receipts)
				revision, revisionErr := s.CatalogRevision(ctx)
				require.NoError(t, revisionErr)
				require.Zero(t, revision)
				require.Zero(t, submitter.writes)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, receipt.AppliedRevision)
			var prices, bindings int
			require.NoError(t, owner.QueryRow(ctx, "SELECT count(*) FROM billing.prices WHERE merchant_id=$1", mid.UUID()).Scan(&prices))
			require.NoError(t, owner.QueryRow(ctx, "SELECT count(*) FROM billing.price_psp_bindings WHERE merchant_id=$1", mid.UUID()).Scan(&bindings))
			require.Equal(t, 6, prices)
			require.Equal(t, 9, bindings, "every declared provider must be retained, including one-time Solana")
			beforeReads, beforeVerifications, beforeChain := providerReads, verifications, chain.reads
			replay, err := s.applyCatalog(ctx, *application, verify)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			require.Equal(t, beforeReads, providerReads)
			require.Equal(t, beforeVerifications, verifications)
			require.Equal(t, beforeChain, chain.reads)
			require.Zero(t, submitter.writes)
		})
	}
}
