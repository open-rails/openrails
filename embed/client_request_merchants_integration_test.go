//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	coreauth "github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// The same SDK performs real catalog operations for two merchants without
// manufacturing merchant clients. The headless graph uses constructor Auth for
// explicit credentials and never requires public HTTP route materialization.
func TestClientRequestMerchantHeadlessAndHTTP(t *testing.T) {
	ctx := t.Context()
	admin, pool, dsn := scopeWithoutRLSDatabase(t)
	newConfig := func() *config.Config {
		return &config.Config{
			Env: "development", TestMode: config.CredentialPostureSandbox,
			MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB,
			CatalogSource: config.CatalogSourceAPI, ProviderWriteMode: config.ProviderWriteModeReadOnly,
			DB: &config.DBConfig{URL: dsn},
		}
	}
	slugs := []string{"request-alpha-" + uuid.NewString(), "request-bravo-" + uuid.NewString()}
	ids := make([]merchant.ID, 2)
	var restricted *embed.Runtime
	for i, slug := range slugs {
		if i == 1 {
			// B's legal slug is A's stable UUID. The explicit selector form,
			// never the spelling, must decide which merchant this call targets.
			slug = ids[0].String()
			slugs[i] = slug
		}
		runtime, id, err := newDeclaredMerchant(ctx, embed.Options{Config: newConfig(), PGXPool: pool, River: embed.RiverFromHost()}, slug, embed.MerchantConfig{DisplayName: slug})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
		ids[i] = id
		if i == 0 {
			restricted = runtime
		}
	}
	operator, staff, machine := uuid.NewString(), uuid.NewString(), uuid.NewString()
	secret := []byte("1048-signed-identity-fixture-key-32bytes")
	issuer := "https://client-request-fixture.test"
	issue := func(subject, kind string) string {
		claims := jwt.MapClaims{"sub": subject, "iss": issuer, "aud": "billing", "exp": time.Now().Add(time.Hour).Unix(), "kind": kind, "roles": []string{"admin"}}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
		require.NoError(t, err)
		return token
	}
	operatorToken, staffToken, machineToken := issue(operator, "user"), issue(staff, "user"), issue(machine, "machine")
	var authenticationCalls atomic.Int64
	var staffAllowed atomic.Bool
	staffAllowed.Store(true)
	var pauseAfterResolution atomic.Bool
	var forwardedTarget billingauth.Target
	resolved, continueWrite := make(chan struct{}), make(chan struct{})
	integration := &billingauth.Integration{
		Authentication: billingauth.AuthenticationFunc(func(_ context.Context, request *http.Request) (billingauth.Identity, error) {
			authenticationCalls.Add(1)
			raw := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
			parsed, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(issuer), jwt.WithAudience("billing"))
			if err != nil || !parsed.Valid {
				return billingauth.Identity{}, billingauth.ErrUnauthenticated
			}
			claims := parsed.Claims.(jwt.MapClaims)
			subject, _ := claims.GetSubject()
			identity := billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: subject, CustomerID: subject, Issuer: issuer, CredentialClass: billingauth.CredentialClassUserSession}
			if claims["kind"] == "machine" {
				identity.Kind = billingauth.Machine
				identity.CredentialClass = "machine"
				identity.Permissions = []string{permissions.MerchantCatalogRead}
			}
			return identity, nil
		}),
		Authorization: billingauth.AuthorizationFunc(func(authContext context.Context, _ *http.Request, identity billingauth.Identity, request billingauth.Requirement) error {
			if request.Scope != billingauth.MerchantScope || request.Target.MerchantID.IsZero() {
				return billingauth.GateError{Status: 403, Message: "wrong scope"}
			}
			if request.Target.MerchantID == forwardedTarget.MerchantID &&
				(request.Target.MerchantSlug != forwardedTarget.MerchantSlug || request.Target.AuthorityGroupID != forwardedTarget.AuthorityGroupID) {
				return billingauth.GateError{Status: 403, Message: "authorization requires canonical merchant identity"}
			}
			for i, id := range ids {
				if request.Target.MerchantID == id && request.Target.MerchantSlug != slugs[i] {
					return billingauth.GateError{Status: 403, Message: "resolved identity mismatch"}
				}
			}
			if request.Permission != permissions.MerchantCatalogRead && request.Permission != permissions.MerchantCatalogUpdate {
				return billingauth.GateError{Status: 403, Message: "operation not granted"}
			}
			if identity.Kind == billingauth.Machine {
				if identity.SubjectID != machine || request.Target.MerchantID != ids[0] || !billingauth.HasPermission(identity.Permissions, request.Permission) {
					return billingauth.GateError{Status: 403, Message: "credential scope denied"}
				}
				return nil
			}
			if identity.SubjectID == operator || (identity.SubjectID == staff && request.Target.MerchantID == ids[0] && staffAllowed.Load()) {
				if request.Permission == permissions.MerchantCatalogUpdate && pauseAfterResolution.CompareAndSwap(true, false) {
					close(resolved)
					select {
					case <-continueWrite:
					case <-authContext.Done():
						return authContext.Err()
					}
				}
				return nil
			}
			return billingauth.GateError{Status: 403, Message: "live permission denied"}
		}),
	}
	newRuntime := func(httpConfig *embed.HTTPConfig) *embed.Runtime {
		runtime, err := embed.New(ctx, embed.Options{Config: newConfig(), PGXPool: pool, River: embed.RiverFromHost(), Auth: integration, HTTP: httpConfig})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
		return runtime
	}
	headless := newRuntime(nil)
	host, err := headless.Client()
	require.NoError(t, err)
	_, err = host.Products.List(ctx, nil)
	require.ErrorIs(t, err, openrails.ErrInvalid, "a headless Client does not invent a merchant")
	defaultHost, err := headless.Client(openrails.WithDefaultMerchant(slugs[0]))
	require.NoError(t, err)
	_, err = defaultHost.Products.List(ctx, nil, openrails.WithMerchant(slugs[1]))
	require.NoError(t, err, "an immutable default is not a deployment restriction")
	require.Zero(t, authenticationCalls.Load(), "private host capability is not an ambient native identity")
	restrictedClient, err := restricted.Client(openrails.WithDefaultMerchant(slugs[1]))
	require.NoError(t, err)
	_, err = restrictedClient.Products.List(ctx, nil)
	require.Error(t, err, "runtime restriction remains effective despite a different Client default")
	_, err = headless.HTTPRoutes()
	require.ErrorContains(t, err, "disabled", "Client operations did not require public HTTP")

	published := newRuntime(&embed.HTTPConfig{MerchantAdmin: true, Catalog: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Merchant: slugs[0], Scope: embed.CustomerBillingManagement}}})
	routes, err := published.HTTPRoutes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" "+route.Path, route.Handler)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	remote, err := openrails.NewRemote(server.URL, openrails.WithAPIKey(operatorToken))
	require.NoError(t, err)

	for _, mode := range []struct {
		name   string
		client *openrails.Client
	}{{"headless", host}, {"http", remote}} {
		t.Run(mode.name, func(t *testing.T) {
			var group errgroup.Group
			var mu sync.Mutex
			products := make([][]*openrails.Product, 2)
			for i := 0; i < 12; i++ {
				group.Go(func() error {
					index := i % 2
					product, err := mode.client.Products.Create(ctx, &openrails.ProductCreateParams{Key: fmt.Sprintf("%s-post-%d", mode.name, i/2), DisplayName: slugs[index]}, openrails.WithMerchant(slugs[index]))
					if err != nil {
						return err
					}
					if product.DisplayName != slugs[index] {
						return fmt.Errorf("cross-merchant response")
					}
					mu.Lock()
					products[index] = append(products[index], product)
					mu.Unlock()
					return nil
				})
			}
			require.NoError(t, group.Wait())
			for i, id := range ids {
				var count int
				require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM billing.products WHERE merchant_id=$1 AND key LIKE $2", id.UUID(), mode.name+"-post-%").Scan(&count))
				require.Equal(t, 6, count, "each request wrote only its resolved merchant")
				owned, err := mode.client.Products.Retrieve(ctx, products[i][0].ID, openrails.ForMerchantID(id))
				require.NoError(t, err)
				require.Equal(t, products[i][0].ID, owned.ID)
				_, err = mode.client.Products.Retrieve(ctx, products[1-i][0].ID, openrails.WithMerchant(slugs[i]))
				require.ErrorIs(t, err, openrails.ErrNotFound, "known IDs cannot cross merchant boundaries")
			}
			_, err := mode.client.Products.List(ctx, nil, openrails.WithMerchant("missing-"+uuid.NewString()))
			require.ErrorIs(t, err, openrails.ErrNotFound)
		})
	}
	for _, mode := range []struct {
		name   string
		client func(string) (*openrails.Client, error)
	}{
		{"headless", func(token string) (*openrails.Client, error) { return headless.Client(openrails.WithAPIKey(token)) }},
		{"http", func(token string) (*openrails.Client, error) {
			return openrails.NewRemote(server.URL, openrails.WithAPIKey(token))
		}},
	} {
		t.Run(mode.name+" credentials", func(t *testing.T) {
			staffAllowed.Store(true)
			staffClient, err := mode.client(staffToken)
			require.NoError(t, err)
			_, err = staffClient.Products.List(ctx, nil, openrails.WithMerchant(slugs[0]))
			require.NoError(t, err)
			_, err = staffClient.Products.List(ctx, nil, openrails.WithMerchant(slugs[1]))
			require.ErrorIs(t, err, openrails.ErrDenied)
			staffAllowed.Store(false)
			_, err = staffClient.Products.List(ctx, nil, openrails.WithMerchant(slugs[0]))
			require.ErrorIs(t, err, openrails.ErrDenied, "same valid JWT cannot retain a revoked permission")
			machineClient, err := mode.client(machineToken)
			require.NoError(t, err)
			_, err = machineClient.Products.List(ctx, nil, openrails.WithMerchant(slugs[0]))
			require.NoError(t, err)
			_, err = machineClient.Products.List(ctx, nil, openrails.WithMerchant(slugs[1]))
			require.ErrorIs(t, err, openrails.ErrDenied)
			_, err = machineClient.Products.Create(ctx, &openrails.ProductCreateParams{Key: "denied", DisplayName: "Denied"}, openrails.WithMerchant(slugs[0]))
			require.ErrorIs(t, err, openrails.ErrDenied)
			invalid, err := mode.client(operatorToken + "invalid")
			require.NoError(t, err)
			_, err = invalid.Products.List(ctx, nil, openrails.WithMerchant(slugs[0]))
			require.ErrorIs(t, err, openrails.ErrUnauthorized, "invalid credentials never fall back to private host authority")
		})
	}

	// Missing resources exercise the actual customer recovery handlers without
	// admitting an operation or contacting a provider. Authentication proves a
	// route-level404 is not being mistaken for the resource's authenticated404.
	for _, mode := range []struct {
		name   string
		client func() (*openrails.Client, error)
	}{
		{"headless", func() (*openrails.Client, error) { return headless.Client(openrails.WithAPIKey(operatorToken)) }},
		{"http", func() (*openrails.Client, error) {
			return openrails.NewRemote(server.URL, openrails.WithAPIKey(operatorToken))
		}},
	} {
		t.Run(mode.name+" recovery routes", func(t *testing.T) {
			client, err := mode.client()
			require.NoError(t, err)
			missingInvoice, missingSubscription := uuid.New(), openrails.SubscriptionID(uuid.New())
			selector := openrails.WithMerchant(slugs[0])
			for name, call := range map[string]func() error{
				"invoice read":      func() error { _, err := client.GetMyInvoice(ctx, missingInvoice, selector); return err },
				"subscription read": func() error { _, err := client.GetMySubscription(ctx, missingSubscription, selector); return err },
				"invoice pay": func() error {
					_, err := client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{InvoiceID: missingInvoice, PaymentMethodID: openrails.PaymentMethodID(uuid.New()), IdempotencyKey: "missing-" + uuid.NewString()}, selector)
					return err
				},
				"subscription retry": func() error {
					_, err := client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{SubscriptionID: missingSubscription, IdempotencyKey: "missing-" + uuid.NewString()}, selector)
					return err
				},
			} {
				t.Run(name, func(t *testing.T) {
					before := authenticationCalls.Load()
					require.ErrorIs(t, call(), openrails.ErrNotFound)
					require.Equal(t, before+1, authenticationCalls.Load(), "the native operation verifies once before the missing-resource refusal")
				})
			}
		})
	}

	t.Run("resolved ID survives slug reuse", func(t *testing.T) {
		oldSlug, renamed := slugs[0], "request-renamed-"+uuid.NewString()
		pauseAfterResolution.Store(true)
		type result struct {
			product *openrails.Product
			err     error
		}
		finished := make(chan result, 1)
		go func() {
			product, err := remote.Products.Create(ctx, &openrails.ProductCreateParams{Key: "resolved-before-rename", DisplayName: "Original merchant"}, openrails.WithMerchant(oldSlug))
			finished <- result{product, err}
		}()
		select {
		case <-resolved:
		case result := <-finished:
			t.Fatalf("request never reached resolved authorization: %v", result.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// Fixture-only rename: the write has already resolved and authorized A.
		// Reuse the old name for B before the handler starts its mutation.
		_, renameErr := admin.Exec(ctx, "UPDATE billing.merchants SET slug=$1 WHERE id=$2", renamed, ids[0].UUID())
		if renameErr == nil {
			_, renameErr = admin.Exec(ctx, "UPDATE billing.merchants SET slug=$1 WHERE id=$2", oldSlug, ids[1].UUID())
		}
		close(continueWrite)
		out := <-finished
		slugs[0], slugs[1] = renamed, oldSlug
		require.NoError(t, renameErr)
		require.NoError(t, out.err)
		var actualMerchant uuid.UUID
		productID, err := openrails.ParseProductID(out.product.ID)
		require.NoError(t, err)
		require.NoError(t, admin.QueryRow(ctx, "SELECT merchant_id FROM billing.products WHERE id=$1", productID.UUID()).Scan(&actualMerchant))
		require.Equal(t, ids[0].UUID(), actualMerchant, "mutation must use the one authorized immutable ID")
		_, err = remote.Products.Retrieve(ctx, out.product.ID, openrails.WithMerchant(oldSlug))
		require.ErrorIs(t, err, openrails.ErrNotFound, "a later lookup uses the new name owner, never the previous merchant")
		_, err = remote.Products.Retrieve(ctx, out.product.ID, openrails.WithMerchant(renamed))
		require.NoError(t, err)
		// The fixed customer audience remains bound to A, not a cached spelling
		// that now belongs to B. Its new current slug still selects A.
		before := authenticationCalls.Load()
		_, err = remote.GetMyInvoice(ctx, uuid.New(), openrails.WithMerchant(renamed))
		require.ErrorIs(t, err, openrails.ErrNotFound)
		require.Equal(t, before+1, authenticationCalls.Load())
		_, err = remote.GetMyInvoice(ctx, uuid.New(), openrails.WithMerchant(oldSlug))
		require.Error(t, err)
		var refusal *openrails.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Contains(t, []int{http.StatusForbidden, http.StatusConflict}, refusal.Status, "name reuse must fail at the configured audience boundary")
		_, err = admin.Exec(ctx, "UPDATE billing.merchants SET status='deleted', deleted_at=now() WHERE id=$1", ids[1].UUID())
		require.NoError(t, err)
		_, err = remote.Products.List(ctx, nil, openrails.WithMerchant(oldSlug))
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = host.Products.List(ctx, nil, openrails.ForMerchantID(ids[1]))
		require.ErrorIs(t, err, openrails.ErrNotFound)
	})

	t.Run("AuthKit former-name forwarding and expiry", func(t *testing.T) {
		// Use the actual naming authority: rename creates a retained claim,
		// GetBySlug returns its canonical name, and expiry permits a new owner.
		require.NoError(t, authcore.ApplyMigrations(ctx, admin, "profiles"))
		naming, err := authcore.New(authcore.Config{
			Keys:      authcore.KeysConfig{VerifyOnly: true},
			Token:     authcore.TokenConfig{Issuer: "https://merchant-names1048.test", IssuedAudiences: []string{"billing"}},
			RBAC:      []authcore.PersonaDef{{Name: "merchant", Parent: coreauth.RootPersona}},
			Ephemeral: authcore.EphemeralConfig{AllowMemory: true},
		}, authcore.Deps{Postgres: admin})
		require.NoError(t, err)
		t.Cleanup(naming.Close)
		core := naming.Client()
		owner, err := core.CreateUser(ctx, "alias-owner@example.test", "alias_owner")
		require.NoError(t, err)
		former, canonical := "former-"+uuid.NewString(), "canonical-"+uuid.NewString()
		groupID, err := core.CreatePermissionGroup(ctx, coreauth.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: former, OwnerSubjectID: owner.ID})
		require.NoError(t, err)
		names, err := authcore.NewGroupDirectory(admin, "profiles")
		require.NoError(t, err)
		t.Cleanup(names.Close)
		authority := controlplane.MerchantNameAuthority(names)
		aliasRuntime := newRuntime(&embed.HTTPConfig{MerchantAdmin: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Merchant: former, Scope: embed.CustomerBillingManagement}}})
		for _, runtime := range []*embed.Runtime{headless, aliasRuntime} {
			app.HostGraph(runtime).Runtime.Merchants.WithNameAuthority(authority)
		}
		directory := app.HostGraph(aliasRuntime).Runtime.Merchants
		original, _, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: former, PermissionGroupID: groupID})
		require.NoError(t, err)
		aliasRoutes, err := aliasRuntime.HTTPRoutes()
		require.NoError(t, err)
		aliasMux := http.NewServeMux()
		for _, route := range aliasRoutes {
			aliasMux.Handle(route.Method+" "+route.Path, route.Handler)
		}
		aliasServer := httptest.NewServer(aliasMux)
		t.Cleanup(aliasServer.Close)
		aliasRemote, err := openrails.NewRemote(aliasServer.URL, openrails.WithAPIKey(operatorToken))
		require.NoError(t, err)
		aliasLocal, err := headless.Client(openrails.WithAPIKey(operatorToken))
		require.NoError(t, err)
		_, err = core.UpdateGroupInstanceAs(ctx, owner.ID, groupID, coreauth.GroupInstanceUpdate{Slug: &canonical})
		require.NoError(t, err)
		forwardedTarget = billingauth.Target{MerchantID: original.ID, MerchantSlug: canonical, AuthorityGroupID: groupID}
		for _, mode := range []struct {
			name   string
			client *openrails.Client
		}{{"headless", aliasLocal}, {"http", aliasRemote}} {
			t.Run(mode.name, func(t *testing.T) {
				product, err := mode.client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "forwarded-" + mode.name, DisplayName: "Forwarded product"}, openrails.WithMerchant(former))
				require.NoError(t, err)
				for _, selector := range []openrails.RequestOption{openrails.WithMerchant(canonical), openrails.ForMerchantID(original.ID)} {
					read, err := mode.client.Products.Retrieve(ctx, product.ID, selector)
					require.NoError(t, err)
					require.Equal(t, product.ID, read.ID)
				}
				before := authenticationCalls.Load()
				_, err = mode.client.GetMyInvoice(ctx, uuid.New(), openrails.WithMerchant(former))
				require.ErrorIs(t, err, openrails.ErrNotFound)
				require.Equal(t, before+1, authenticationCalls.Load(), "forwarded customer request reaches authenticated handler")
			})
		}
		// Advance only this fixture's claim deadline; all lookup and reclaim
		// decisions continue through the real AuthKit directory and Client.
		_, err = admin.Exec(ctx, "UPDATE profiles.name_claims SET expires_at=now()-interval '1 second' WHERE owner_id=$1::uuid AND name=$2", groupID, former)
		require.NoError(t, err)
		for _, client := range []*openrails.Client{aliasLocal, aliasRemote} {
			_, err = client.Products.List(ctx, nil, openrails.WithMerchant(former))
			require.ErrorIs(t, err, openrails.ErrNotFound, "expired alias must not fall back to stale billing projection")
			_, err = client.Products.List(ctx, nil, openrails.WithMerchant(canonical))
			require.NoError(t, err)
		}
		replacementGroup, err := core.CreatePermissionGroup(ctx, coreauth.CreatePermissionGroupRequest{Persona: "merchant", InstanceSlug: former, OwnerSubjectID: owner.ID})
		require.NoError(t, err)
		replacement, _, err := directory.Provision(ctx, merchants.ProvisionRequest{Slug: former, PermissionGroupID: replacementGroup})
		require.NoError(t, err)
		require.NotEqual(t, original.ID, replacement.ID)
		_, err = aliasRemote.GetMyInvoice(ctx, uuid.New(), openrails.WithMerchant(former))
		var refusal *openrails.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, http.StatusConflict, refusal.Status, "reassigned claim cannot change a fixed customer audience")
		_, err = aliasRemote.GetMyInvoice(ctx, uuid.New(), openrails.WithMerchant(canonical))
		require.ErrorIs(t, err, openrails.ErrNotFound)
	})
}
