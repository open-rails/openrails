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
	// payment page sells card subscriptions and one-time sales through it.
	CardPSPKey = "cards"
	// CardTokenizationKey is public; the browser's Collect.js is the test's.
	CardTokenizationKey = "e2e-tokenization"
)

type Runtime struct {
	Auth       *authkit.Client
	StaffRoles map[string]iam.Role
	// Engine is the embedded engine; Client is its merchant's handle.
	Engine  *openrails.Engine
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
	cranker, treasury := solanago.NewWallet().PrivateKey, solanago.NewWallet().PublicKey()
	authCfg, staffRoles := authConfig(baseURL)
	auth, err := authkit.New(ctx, authCfg, authkit.Deps{Postgres: pool})
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
		ProviderWriteMode:    openrails.ProviderWritesFull,
		PublicBillingBaseURL: baseURL + "/billing",
		ReturnOrigins:        []string{baseURL},
		ProviderSandbox:      &openrails.ProviderSandboxConfig{SolanaRPCURL: chain.URL(), NMIGatewayURL: gateway.URL()},
		Solana:               &openrails.SolanaConfig{Crankers: []openrails.SolanaCrankerConfig{{Mode: "local_keypair", PrivateKey: cranker.String()}}},
		// AuthKit's issuer is baseURL: its users are this merchant's customers.
		Merchants: map[string]openrails.MerchantDeclaration{MerchantSlug: {Issuer: baseURL, DisplayName: "billing-ui e2e", MerchantSettings: billing.MerchantSettings{PSPRouting: &pspRouting}, PSPs: openrails.PSPs{
			// An armed Solana PSP on devnet: purchase offers it.
			SolanaPSPKey: openrails.SolanaPSP{Environment: openrails.EnvironmentDevnet, Treasury: treasury.String()}.PSPConfig(),
			CardPSPKey: openrails.NMIPSP{Environment: openrails.EnvironmentSandbox,
				GatewayID:            "billing-ui-e2e-cards",
				SecurityKey:          "e2e-security-key",
				WebhookSigningSecret: "e2e-webhook-secret",
				TokenizationKey:      CardTokenizationKey,
			}.PSPConfig(),
		}}},
		// Every browser of the suite is one address, 127.0.0.1, which one
		// purchase limit would soon refuse.
		RateLimitsDisabled: true,
	}
	engine, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: pool})
	if err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	defer func() {
		if err != nil {
			_ = engine.Close(context.Background())
		}
	}()
	client, err := engine.MerchantBySlug(MerchantSlug)
	if err != nil {
		return nil, err
	}
	local, err := engine.LocalMerchant(client.MerchantID())
	if err != nil {
		return nil, err
	}
	if _, err := local.DeclarePSP(ctx, billing.PSPDeclaration{Key: PSPKey, Rail: PSPRail, Environment: openrails.EnvironmentSandbox, PSPAccount: billing.PSPAccount{GatewayID: PSPAccountID}}); err != nil {
		return nil, err
	}
	if workers {
		if err := engine.Start(ctx); err != nil {
			return nil, err
		}
	}
	catalog, err := seedCatalog(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("seed catalog: %w", err)
	}
	catalog.MerchantID = client.MerchantID().String()
	if err := armDestructive(ctx, pool); err != nil {
		return nil, fmt.Errorf("arm destructive actions: %w", err)
	}
	return &Runtime{Auth: auth, StaffRoles: staffRoles, Engine: engine, Client: client, Catalog: catalog, Solana: chain, NMI: gateway, BaseURL: baseURL}, nil
}

// billingRoutes is the billing surface the storefront and account pages call.
var billingRoutes = openrails.Routes{Prefix: "/billing", RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true}}

// authConfig is AuthKit's configuration: its JSON API at /auth/v1 beside
// OpenRails at /billing, open registration and no second factor.
func authConfig(issuer string) (authkit.Config, map[string]iam.Role) {
	roles := authkit.NewRoles(authkit.CustomRoles)
	merchant := roles.Persona(billing.MerchantGroupPersona, authkit.CustomRoles)
	var grants []iam.Grant
	var read iam.Perm
	for _, permission := range billing.Permissions() {
		if permission.Persona() == billing.MerchantGroupPersona {
			merchant.Permission(permission.Resource(), permission.Action())
			continue
		}
		grant := roles.Root.Permission(permission.Resource(), permission.Action())
		grants = append(grants, grant)
		if permission == billing.RootCustomerRead {
			read = grant
		}
	}
	staff := map[string]iam.Role{"admin": roles.Root.Role("billing-admin", grants...), "reader": roles.Root.Role("billing-reader", read)}
	return authkit.Config{
		Roles:        roles,
		Database:     authkit.DatabaseConfig{Schema: AuthSchema, RiverSchema: AuthSchema},
		HTTP:         &authkit.HTTPConfig{DirectPeerIP: true, APIPath: "/auth"},
		Token:        authkit.TokenConfig{Issuer: issuer, IssuedAudiences: []string{Audience}},
		Registration: authkit.RegistrationConfig{NativeUserMode: iam.RegistrationModeOpen, Verification: iam.RegistrationVerificationNone},
		Keys:         authkit.KeysConfig{AllowEphemeralDevKeys: true},
		TwoFactor:    authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},
	}, staff
}

// pspRouting sells the card product through the card PSP and everything
// else through Solana; the declared-only NMI account never sells.
var pspRouting = []billing.PSPRoutingRule{
	{Match: billing.PSPRoutingMatch{Product: "e2e-card"}, Prefer: []string{CardPSPKey}},
	{Prefer: []string{SolanaPSPKey}},
}

// Mount registers AuthKit at /auth/v1 and OpenRails at /billing on mux,
// guarded by AuthKit.
func (r *Runtime) Mount(mux *http.ServeMux) error {
	if err := r.Auth.Mount(mux); err != nil {
		return err
	}
	routes := billingRoutes
	routes.Auth = r.Auth.Authenticator() // says who a request is; OpenRails decides what to admit
	return openrailshttp.Mount(mux, r.Engine, routes)
}

func (r *Runtime) Close() {
	_ = r.Engine.Close(context.Background())
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
