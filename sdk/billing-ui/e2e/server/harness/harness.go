// Package harness composes real AuthKit + embedded OpenRails on Postgres for
// billing-ui's e2e suite and contract generator, so both see the same mount.
package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	authkithttp "github.com/open-rails/authkit/adapters/http"
	"github.com/open-rails/authkit/authhttp"
	"github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	openrailsconfig "github.com/open-rails/openrails/config"
	openrailsembed "github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/open-rails/openrails/nmimock"
	"github.com/open-rails/openrails/pkg/billingauth"
)

const (
	AuthSchema    = "profiles"
	BillingSchema = "billing"
	Audience      = "billing-ui-e2e"
	MerchantSlug  = "billing-ui-e2e"
	// Imported facts are attributed to a credential-less, declared-only PSP.
	// NMI: its cancel defers the remote delete, so resume is exercisable.
	PSPKey       = "nmi"
	PSPRail      = "nmi"
	PSPAccountID = "billing-ui-e2e"
	// ManagePrefix mounts the CustomerBillingManagement scope beside /v1/me.
	ManagePrefix = "/v1/manage"
	SolanaPSPKey = "solana"
	// CardPSPKey is an armed NMI account on the loopback gateway (nmimock):
	// hosted checkout sells card subscriptions and one-time sales through it.
	CardPSPKey = "cards"
	// CardTokenizationKey is public; the browser's Collect.js is the test's.
	CardTokenizationKey = "e2e-tokenization"
)

type Runtime struct {
	Auth    *embedded.Runtime
	Billing *openrailsembed.Runtime
	Client  *openrails.Client
	Catalog Catalog
	// Solana is the loopback chain the armed Solana PSP reads.
	Solana *solanafake.Node
	// NMI is the loopback gateway the armed card PSP charges.
	NMI     *nmimock.Mock
	BaseURL string
}

// Open connects to dsn and applies AuthKit and OpenRails migrations.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("harness: Postgres DSN is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := embedded.ApplyMigrations(ctx, pool, AuthSchema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("authkit migrations: %w", err)
	}
	if err := openrailsembed.ApplyMigrations(ctx, pool, openrailsembed.MigrationOptions{Schema: BillingSchema}); err != nil {
		pool.Close()
		return nil, fmt.Errorf("openrails migrations: %w", err)
	}
	return pool, nil
}

// New builds both runtimes and seeds the catalog. The server runs River
// workers because customer cancel/resume are queued jobs.
func New(ctx context.Context, baseURL, dsn string, pool *pgxpool.Pool, workers bool) (_ *Runtime, err error) {
	chain := solanafake.New()
	gateway := nmimock.New(nmimock.Options{})
	defer func() {
		if err != nil {
			chain.Close()
			gateway.Close()
		}
	}()
	signer := solanago.NewWallet().PrivateKey
	auth, err := embedded.New(embedded.Config{
		Schema: AuthSchema,
		HTTP:   authhttp.Config{DirectPeerIP: true, PerProcessRateLimits: true, Mount: authhttp.MountOptions{APIPrefix: "/auth/v1"}},
		Token: embedded.TokenConfig{
			Issuer:            baseURL,
			IssuedAudiences:   []string{Audience},
			ExpectedAudiences: []string{Audience},
		},
		Registration: embedded.RegistrationConfig{
			NativeUserMode: embedded.RegistrationModeOpen,
			Verification:   embedded.RegistrationVerificationNone,
		},
		Keys: embedded.KeysConfig{AllowEphemeralDevKeys: true},

		TwoFactor: embedded.TwoFactorConfig{Mode: embedded.TwoFactorDisabled},
	}, embedded.Deps{Postgres: pool})
	if err != nil {
		return nil, fmt.Errorf("authkit: %w", err)
	}
	defer func() {
		if err != nil {
			auth.Close()
		}
	}()
	identity, err := billingauth.NewIntegration(billingauth.IntegrationOptions{Verifier: auth.Verifier(), Customer: billingauth.SubjectCustomerID})
	if err != nil {
		return nil, err
	}
	billing, err := openrailsembed.New(ctx, openrailsembed.Options{
		Auth: identity,
		HTTP: &openrailsembed.HTTPConfig{CustomerRoutes: []openrailsembed.CustomerRoutesConfig{
			{Merchant: MerchantSlug, Scope: openrailsembed.CustomerSelfService},
			{Merchant: MerchantSlug, Scope: openrailsembed.CustomerBillingManagement, Prefix: ManagePrefix},
		}},
		Merchant: &openrailsembed.MerchantDeclaration{
			Slug: MerchantSlug,
			Config: openrailsembed.MerchantConfig{DisplayName: "billing-ui e2e", CheckoutRouting: checkoutRouting, PSPs: map[string]openrailsembed.PSPConfig{
				// An armed Solana PSP on devnet: checkout offers it (#1078).
				SolanaPSPKey: {"solana": {
					Signer:   &openrailsembed.PSPSignerConfig{Mode: "local_keypair"},
					Secrets:  map[string]string{"private_key": signer.String()},
					Settings: map[string]any{"rpc_provider": "public", "tokens": map[string]any{"SOL": map[string]any{}, "DUSD": map[string]any{}}},
				}},
				CardPSPKey: {"nmi": {
					AccountID: "billing-ui-e2e-cards",
					Secrets:   map[string]string{"security_key": "e2e-security-key", "webhook_signing_secret": "e2e-webhook-secret"},
					Settings:  map[string]any{"tokenization_key": CardTokenizationKey},
				}},
			}},
			PSPs: []openrailsembed.PSPDeclaration{{Key: PSPKey, Rail: PSPRail, AccountID: PSPAccountID}},
		},
		Config: &openrailsconfig.Config{
			TestMode:             openrailsconfig.CredentialPostureSandbox,
			ProviderWriteMode:    openrailsconfig.ProviderWriteModeFull,
			AllowCatalogUpdates:  true,
			DB:                   &openrailsconfig.DBConfig{URL: dsn, Schema: BillingSchema},
			PublicBillingBaseURL: baseURL + "/billing",
			ReturnOrigins:        []string{baseURL},
			ProviderSandbox:      &openrailsconfig.ProviderSandboxConfig{SolanaRPCURL: chain.URL(), NMIGatewayURL: gateway.URL()},
		},
		PGXPool:    pool,
		RunWorkers: workers,
	})
	if err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	defer func() {
		if err != nil {
			_ = billing.Close(context.Background())
		}
	}()
	client, err := billing.Client()
	if err != nil {
		return nil, err
	}
	catalog, err := seedCatalog(ctx, client, chain, signer.PublicKey())
	if err != nil {
		return nil, fmt.Errorf("seed catalog: %w", err)
	}
	if err := armDestructive(ctx, pool); err != nil {
		return nil, fmt.Errorf("arm destructive actions: %w", err)
	}
	return &Runtime{Auth: auth, Billing: billing, Client: client, Catalog: catalog, Solana: chain, NMI: gateway, BaseURL: baseURL}, nil
}

// checkoutRouting sells the card product through the card PSP and everything
// else through Solana; the declared-only NMI account never sells.
var checkoutRouting = []openrailsembed.CheckoutRoutingRuleConfig{
	{Match: openrailsembed.CheckoutRoutingMatchConfig{Product: "e2e-card"}, Prefer: []string{CardPSPKey}},
	{Prefer: []string{SolanaPSPKey}},
}

// BillingRoutes returns the embedded billing routes, relative to /billing.
func (r *Runtime) BillingRoutes() ([]openrailsembed.HTTPRoute, error) {
	return r.Billing.HTTPRoutes()
}

// Mount registers AuthKit at /auth/v1 and OpenRails at /billing on mux.
func (r *Runtime) Mount(mux *http.ServeMux) error {
	authRoutes, err := authkithttp.Routes(r.Auth)
	if err != nil {
		return err
	}
	if err := authRoutes.Mount(mux); err != nil {
		return err
	}
	billing, err := openrailshttp.Routes(r.Billing)
	if err != nil {
		return err
	}
	return billing.Mount(mux, "/billing")
}

func (r *Runtime) Close() {
	_ = r.Billing.Close(context.Background())
	r.Auth.Close()
	r.Solana.Close()
	r.NMI.Close()
}

// armDestructive is the operator arming a reviewed deployment
// (docs/operations.md): member cancels of provider-billed subscriptions are
// refused until provider deletes may run.
func armDestructive(ctx context.Context, pool *pgxpool.Pool) error {
	schema := pgx.Identifier{BillingSchema}.Sanitize()
	if _, err := pool.Exec(ctx, `UPDATE `+schema+`.destructive_action_switch SET enabled = true, updated_by = 'billing-ui-e2e'`); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, `INSERT INTO `+schema+`.merchant_destructive_policy (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason)
		SELECT id, true, now(), 'billing-ui-e2e', 'e2e deployment' FROM `+schema+`.merchants WHERE slug = $1
		ON CONFLICT (merchant_id) DO UPDATE SET enforce_armed_at = now(), destructive_actions_enabled = true`, MerchantSlug)
	return err
}
