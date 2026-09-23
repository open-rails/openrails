// Package harness composes real AuthKit + embedded OpenRails on Postgres for
// billing-ui's e2e suite and contract generator, so both see the same mount.
package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	authkithttp "github.com/open-rails/authkit/adapters/http"
	"github.com/open-rails/authkit/authhttp"
	"github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	openrailsconfig "github.com/open-rails/openrails/config"
	openrailsembed "github.com/open-rails/openrails/embed"
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
)

type Runtime struct {
	Auth    *embedded.Runtime
	Billing *openrailsembed.Runtime
	Client  *openrails.Client
	Catalog Catalog
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
	auth, err := embedded.New(embedded.Config{
		Schema: AuthSchema,
		HTTP:   authhttp.Config{DirectPeerIP: true, Mount: authhttp.MountOptions{APIPrefix: "/auth/v1"}},
		Token: embedded.TokenConfig{
			Issuer:            baseURL,
			IssuedAudiences:   []string{Audience},
			ExpectedAudiences: []string{Audience},
		},
		Registration: embedded.RegistrationConfig{
			NativeUserMode: embedded.RegistrationModeOpen,
			Verification:   embedded.RegistrationVerificationNone,
		},
		Keys:      embedded.KeysConfig{AllowEphemeralDevKeys: true},
		Ephemeral: embedded.EphemeralConfig{AllowMemory: true},
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
			Slug:   MerchantSlug,
			Config: openrailsembed.MerchantConfig{DisplayName: "billing-ui e2e"},
			PSPs:   []openrailsembed.PSPDeclaration{{Key: PSPKey, Rail: PSPRail, AccountID: PSPAccountID}},
		},
		Config: &openrailsconfig.Config{
			TestMode:            openrailsconfig.CredentialPostureSandbox,
			ProviderWriteMode:   openrailsconfig.ProviderWriteModeFull,
			AllowCatalogUpdates: true,
			DB:                  &openrailsconfig.DBConfig{URL: dsn, Schema: BillingSchema},
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
	catalog, err := seedCatalog(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("seed catalog: %w", err)
	}
	return &Runtime{Auth: auth, Billing: billing, Client: client, Catalog: catalog}, nil
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
}
