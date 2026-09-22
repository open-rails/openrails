//go:build integration

package hosttools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/merchants"
	catalogmodule "github.com/open-rails/openrails/internal/modules/catalog"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Construct exactly the minimal runtime used by apply-catalog, then direct its
// existing RPC endpoint seam to a local server. The whole consumer artifact
// still goes through ApplyMerchantCatalog and real provider reference checks.
func TestCatalogCLIRuntimeVerifiesWholeArtifactWithoutSigner(t *testing.T) {
	pool := dbtest.SharedSuperuserPGXPool(t)
	for _, source := range []string{config.MerchantConfigSourceManifest, config.MerchantConfigSourceAPI} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%t", source, missing), func(t *testing.T) {
				ctx := t.Context()
				mid := merchant.ID(uuid.New())
				slug := "catalog-reader-" + uuid.NewString()
				_, err := pool.Exec(ctx, `INSERT INTO billing.merchants(id,slug,status) VALUES($1,$2,'active')`, mid.UUID(), slug)
				require.NoError(t, err)
				owner := solanago.NewWallet().PublicKey()
				nmiID := "nmi-" + mid.String()
				for _, account := range []struct{ key, rail, id string }{{"mobius", "nmi", nmiID}, {"ccbill", "ccbill", "123456-0001-" + mid.String()}, {"solana", "solana", owner.String()}} {
					settings := `{}`
					if account.rail == "solana" {
						settings = `{"settings":{"rpc_provider":"public","tokens":{"DUSD":{}}}}`
					}
					_, err = pool.Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,key,rail,environment,account_id,evidence) VALUES($1,$2,$3,$4,'test',$5,$6::jsonb)`, uuid.New(), mid.UUID(), account.key, account.rail, account.id, settings)
					require.NoError(t, err)
				}
				keyName, err := merchants.PSPSecretName("nmi", "test", nmiID, "security_key")
				require.NoError(t, err)
				dbKey := "configured-reader-key"
				if source == config.MerchantConfigSourceManifest {
					dbKey = "must-not-fall-back-to-db"
				}
				_, err = pool.Exec(ctx, `INSERT INTO billing.merchant_secrets(merchant_id,name,value,version) VALUES($1,$2,$3,1)`, mid.UUID(), keyName, dbKey)
				require.NoError(t, err)
				// Deliberately unusable private material proves address discovery
				// does not construct a signer or read a signing credential.
				privateName, err := merchants.PSPSecretName("solana", "test", owner.String(), "private_key")
				require.NoError(t, err)
				_, err = pool.Exec(ctx, `INSERT INTO billing.merchant_secrets(merchant_id,name,value,version) VALUES($1,$2,'not-a-signing-key',1)`, mid.UUID(), privateName)
				require.NoError(t, err)
				var nmiReads atomic.Int32
				nmi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, http.MethodGet, r.Method)
					nmiReads.Add(1)
					amount := "23.00"
					if r.URL.Path == "/plans/premium" {
						amount = "19.00"
					}
					require.Contains(t, []string{"/plans/premium", "/plans/premium_new"}, r.URL.Path)
					fmt.Fprintf(w, `{"object":"plan","id":%q,"plan_amount":%q,"day_frequency":"30","plan_payments":"0"}`, strings.TrimPrefix(r.URL.Path, "/plans/"), amount)
				}))
				t.Cleanup(nmi.Close)
				cfg := &config.Config{Env: "development", TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, MerchantConfigSource: source, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}, ProviderSandbox: &config.ProviderSandboxConfig{NMIGatewayURL: nmi.URL}}
				manifestPath := ""
				if source == config.MerchantConfigSourceManifest {
					manifestPath = filepath.Join(t.TempDir(), "merchants.yaml")
					snapshot := fmt.Sprintf("version: 1\nmerchants:\n  %s:\n    display_name: Catalog reader\n    psps:\n      mobius:\n        nmi:\n          account_id: %s\n          secrets: {security_key: configured-reader-key}\n", slug, nmiID)
					require.NoError(t, os.WriteFile(manifestPath, []byte(snapshot), 0600))
				}
				opts := CatalogApplyOptions{Config: cfg, PGXPool: pool, Merchant: slug, MerchantManifestPath: manifestPath}
				rt, _, cleanup, err := catalogRuntime(ctx, opts)
				require.NoError(t, err)
				t.Cleanup(cleanup)
				require.Nil(t, rt.SolanaCranker)
				require.Nil(t, rt.SolanaPayPoller)
				require.Nil(t, rt.RiverClient)
				require.Nil(t, rt.SubscriptionLifecycleService)
				require.Equal(t, config.ProviderWriteModeFull, cfg.ProviderWriteMode, "reader construction never changes caller configuration")
				scoped := merchant.WithID(ctx, mid)
				rail, err := rt.RailConfigs.RailConfig(scoped, "nmi", nmiID)
				require.NoError(t, err)
				require.Equal(t, "configured-reader-key", rail.NMI.SecurityKey)
				address, err := rt.SolanaPlanService.MerchantAddress(scoped, mid)
				require.NoError(t, err)
				require.Equal(t, owner, address)
				mintText := solanatokens.ForNetwork("devnet")["DUSD"].Mint
				mint := solanago.MustPublicKeyFromBase58(mintText)
				content := catalogmodule.OpenRailsPriceContentKey("premium", "usd", 23000000, nil) + ".h720:" + mintText
				digest := sha256.Sum256([]byte(content))
				planID := binary.BigEndian.Uint64(digest[:8])
				pda, bump, err := subscriptions.DerivePlanPDA(owner, planID)
				require.NoError(t, err)
				plan := make([]byte, subscriptions.PlanAccountSize)
				plan[0] = 1
				copy(plan[1:33], owner[:])
				plan[33] = bump
				plan[34] = subscriptions.PlanStatusActive
				binary.LittleEndian.PutUint64(plan[35:43], planID)
				copy(plan[43:75], mint[:])
				binary.LittleEndian.PutUint64(plan[75:83], 23000000)
				binary.LittleEndian.PutUint64(plan[83:91], 720)
				binary.LittleEndian.PutUint64(plan[91:99], 1700000000)
				mintData := make([]byte, solanaint.MintAccountSize)
				mintData[44] = 6
				mintData[45] = 1
				var rpcReads atomic.Int32
				rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req struct {
						ID     json.RawMessage   `json:"id"`
						Method string            `json:"method"`
						Params []json.RawMessage `json:"params"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					require.Equal(t, "getAccountInfo", req.Method, "no signing or submission RPC is allowed")
					rpcReads.Add(1)
					var account string
					require.NoError(t, json.Unmarshal(req.Params[0], &account))
					require.Contains(t, []string{pda.String(), mint.String()}, account)
					raw := plan
					accountOwner := subscriptions.ProgramID.String()
					if account == mint.String() {
						raw = mintData
						accountOwner = solanago.TokenProgramID.String()
					}
					var value any = map[string]any{"data": []string{base64.StdEncoding.EncodeToString(raw), "base64"}, "executable": false, "lamports": 1, "owner": accountOwner, "rentEpoch": 0}
					if missing && account == pda.String() {
						value = nil
					}
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"context": map[string]any{"slot": 1}, "value": value}}))
				}))
				t.Cleanup(rpc.Close)
				rt.SolanaRPCResolver.Endpoint = rpc.URL
				rpcClient, err := rt.SolanaRPCResolver.Resolve(scoped, mid)
				require.NoError(t, err)
				_, err = rpcClient.SendTransaction(scoped, nil)
				require.ErrorIs(t, err, solanaint.ErrProviderReadOnly)
				require.Zero(t, rpcReads.Load())
				raw, err := os.ReadFile(filepath.Join("..", "service", "testdata", "doujins-catalog-application.yaml"))
				require.NoError(t, err)
				opts.App = &app.App{Runtime: rt}
				opts.Manifest = raw
				receipt, err := ApplyMerchantCatalog(ctx, opts)
				if missing {
					require.ErrorContains(t, err, "missing")
					var products, receipts int
					require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.products WHERE merchant_id=$1),(SELECT count(*) FROM billing.catalog_applications WHERE merchant_id=$1)`, mid.UUID()).Scan(&products, &receipts))
					require.Zero(t, products)
					require.Zero(t, receipts)
					return
				}
				require.NoError(t, err)
				require.NotNil(t, receipt)
				require.EqualValues(t, 2, nmiReads.Load())
				require.EqualValues(t, 2, rpcReads.Load())
				var prices, bindings int
				require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.prices WHERE merchant_id=$1),(SELECT count(*) FROM billing.price_psp_bindings WHERE merchant_id=$1)`, mid.UUID()).Scan(&prices, &bindings))
				require.Equal(t, 6, prices)
				require.Equal(t, 9, bindings)
				replay, err := ApplyMerchantCatalog(ctx, opts)
				require.NoError(t, err)
				require.True(t, replay.Replayed)
				require.EqualValues(t, 2, nmiReads.Load())
				require.EqualValues(t, 2, rpcReads.Load())
			})
		}
	}
}

func TestCatalogCLIRuntimeRefusesUnavailableCredentialPlane(t *testing.T) {
	pool := dbtest.SharedSuperuserPGXPool(t)
	id := uuid.New()
	slug := "catalog-vault-" + id.String()
	_, err := pool.Exec(t.Context(), `INSERT INTO billing.merchants(id,slug,status) VALUES($1,$2,'active')`, id, slug)
	require.NoError(t, err)
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	t.Cleanup(vault.Close)
	cfg := &config.Config{Env: "development", DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendVault, Vault: &config.VaultConfig{Enabled: true, Address: vault.URL, AuthMethod: "token", Token: "fixture-token"}}
	_, _, _, err = catalogRuntime(t.Context(), CatalogApplyOptions{Config: cfg, PGXPool: pool, Merchant: slug})
	require.ErrorContains(t, err, "credential plane unavailable")
	cfg.MerchantConfigSource = config.MerchantConfigSourceManifest
	cfg.Vault = nil
	_, _, _, err = catalogRuntime(t.Context(), CatalogApplyOptions{Config: cfg, PGXPool: pool, Merchant: slug, MerchantManifestPath: filepath.Join(t.TempDir(), "missing.yaml")})
	require.ErrorContains(t, err, "read merchant manifest")
	require.NoError(t, pool.Ping(context.Background()), "cleanup must not close a borrowed host pool")
}
