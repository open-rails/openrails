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
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/solanafake"
)

const (
	AuthSchema    = "profiles"
	BillingSchema = "billing"
	Audience      = "billing-ui-e2e"
	MerchantSlug  = "billing-ui-e2e"
	// PSPKey is the credential-less, declared-only NMI PSP imported facts are
	// attributed to; its cancel defers the remote delete, so resume is exercisable.
	PSPKey       = "nmi"
	PSPRail      = "nmi"
	PSPAccountID = "billing-ui-e2e"
	SolanaPSPKey = "solana"
	// CardPSPKey is an armed NMI account on the loopback gateway (nmimock):
	// hosted checkout sells card subscriptions and one-time sales through it.
	CardPSPKey = "cards"
	// CardTokenizationKey is public; the browser's Collect.js is the test's.
	CardTokenizationKey = "e2e-tokenization"
)

type Runtime struct {
	Auth    *authkit.Client
	Client  *openrails.Client
	Catalog Catalog
	// Solana is the loopback chain the armed Solana PSP reads.
	Solana *solanafake.Node
	// NMI is the loopback gateway the armed card PSP charges.
	NMI     *nmimock.Mock
	BaseURL string
}

// Open connects to dsn; authkit.New and openrails.New create their own
// tables.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("harness: Postgres DSN is required")
	}
	return pgxpool.New(ctx, dsn)
}

// New builds both runtimes and seeds the catalog. The server runs River
// workers because customer cancel/resume are queued jobs. pageURL is the
// hosted checkout page, served on another origin and framed by baseURL; ""
// is the single-site case.
func New(ctx context.Context, baseURL, pageURL, dsn string, pool *pgxpool.Pool, workers bool) (_ *Runtime, err error) {
	chain := solanafake.New()
	gateway := nmimock.New(nmimock.Options{})
	defer func() {
		if err != nil {
			chain.Close()
			gateway.Close()
		}
	}()
	signer := solanago.NewWallet().PrivateKey
	auth, err := authkit.New(ctx, authConfig(baseURL), authkit.Deps{Postgres: pool})
	if err != nil {
		return nil, fmt.Errorf("authkit: %w", err)
	}
	defer func() {
		if err != nil {
			_ = auth.Close(context.Background())
		}
	}()
	cfg := openrails.Config{
		Database:             openrails.DatabaseConfig{Schema: BillingSchema},
		TestMode:             openrails.Sandbox,
		ProviderWriteMode:    openrails.ProviderWritesFull,
		PublicBillingBaseURL: baseURL + "/billing",
		ReturnOrigins:        []string{baseURL},
		ProviderSandbox:      &openrails.ProviderSandboxConfig{SolanaRPCURL: chain.URL(), NMIGatewayURL: gateway.URL()},
		Merchant: openrails.MerchantDeclaration{
			Slug: MerchantSlug, DisplayName: "billing-ui e2e", Settings: billing.MerchantSettings{CheckoutRouting: &checkoutRouting},
			PSPs: map[string]openrails.PSPConfig{
				// An armed Solana PSP on devnet: checkout offers it.
				SolanaPSPKey: openrails.SolanaPSP{
					PrivateKey:  signer.String(),
					RPCProvider: "public",
					Tokens:      map[string]openrails.SolanaToken{"SOL": {}, "DUSD": {}},
				}.PSPConfig(),
				CardPSPKey: openrails.NMIPSP{
					AccountID:            "billing-ui-e2e-cards",
					SecurityKey:          "e2e-security-key",
					WebhookSigningSecret: "e2e-webhook-secret",
					TokenizationKey:      CardTokenizationKey,
				}.PSPConfig(),
			},
		},
		Checkout: checkoutConfig(baseURL, pageURL),
		// Every browser of the suite is one address, 127.0.0.1, which one
		// checkout limit would soon refuse.
		RateLimitsDisabled: true,
	}
	client, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool})
	if err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	defer func() {
		if err != nil {
			_ = client.Close(context.Background())
		}
	}()
	if _, err := client.DeclarePSP(ctx, client.MerchantID(), billing.PSPDeclaration{Key: PSPKey, Rail: PSPRail, AccountID: PSPAccountID}); err != nil {
		return nil, err
	}
	if workers {
		if err := client.Start(ctx); err != nil {
			return nil, err
		}
	}
	catalog, err := seedCatalog(ctx, client, chain, signer.PublicKey())
	if err != nil {
		return nil, fmt.Errorf("seed catalog: %w", err)
	}
	if err := armDestructive(ctx, pool); err != nil {
		return nil, fmt.Errorf("arm destructive actions: %w", err)
	}
	return &Runtime{Auth: auth, Client: client, Catalog: catalog, Solana: chain, NMI: gateway, BaseURL: baseURL}, nil
}

// checkoutConfig serves the payment page at pageURL, framed by the app at
// baseURL.
func checkoutConfig(baseURL, pageURL string) openrails.CheckoutConfig {
	if pageURL == "" {
		return openrails.CheckoutConfig{}
	}
	return openrails.CheckoutConfig{PageURL: pageURL, EmbedOrigins: []string{baseURL}}
}

// billingRoutes is the billing surface the storefront and account pages call.
var billingRoutes = openrails.Routes{Prefix: "/billing"}

// authConfig is AuthKit's configuration: its JSON API at /auth/v1 beside
// OpenRails at /billing, open registration and no second factor.
func authConfig(issuer string) authkit.Config {
	return authkit.Config{
		Database:     authkit.DatabaseConfig{Schema: AuthSchema, RiverSchema: AuthSchema},
		HTTP:         &authkit.HTTPConfig{DirectPeerIP: true, APIPath: "/auth"},
		Token:        authkit.TokenConfig{Issuer: issuer, IssuedAudiences: []string{Audience}},
		Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationNone},
		Keys:         authkit.KeysConfig{AllowEphemeralDevKeys: true},
		TwoFactor:    authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},
	}
}

// checkoutRouting sells the card product through the card PSP and everything
// else through Solana; the declared-only NMI account never sells.
var checkoutRouting = []billing.CheckoutRoutingRule{
	{Match: billing.CheckoutRoutingMatch{Product: "e2e-card"}, Prefer: []string{CardPSPKey}},
	{Prefer: []string{SolanaPSPKey}},
}

// Mount registers AuthKit at /auth/v1 and OpenRails at /billing on mux,
// guarded by AuthKit.
func (r *Runtime) Mount(mux *http.ServeMux) error {
	if err := r.Auth.Mount(mux); err != nil {
		return err
	}
	routes := billingRoutes
	routes.Auth = customerAuth{r.Auth}
	return openrailshttp.Mount(mux, r.Client, routes)
}

// customerAuth is AuthKit as OpenRails' Auth. Its users hold tokens minted
// outside a sign-in (CreateUser), so Required verifies the token without a
// session; the rest is AuthKit's own.
type customerAuth struct{ *authkit.Client }

func (a customerAuth) Required() func(http.Handler) http.Handler { return verify.Required(a.Client) }

// PaymentPage wraps the handler serving the hosted checkout page.
func (r *Runtime) PaymentPage(page http.Handler) http.Handler {
	return openrailshttp.CheckoutFramePolicy(r.Client)(page)
}

func (r *Runtime) Close() {
	_ = r.Client.Close(context.Background())
	_ = r.Auth.Close(context.Background())
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
