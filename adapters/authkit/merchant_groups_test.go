package authkit_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	native "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	adapter "github.com/open-rails/openrails/adapters/authkit"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/archivewire"
	billingauthtest "github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/stretchr/testify/require"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExplicitMerchantTenantBinding(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_E2E_DSN")
	if dsn == "" {
		t.Skip("OPENRAILS_E2E_DSN supplies an isolated PostgreSQL test database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	authSchema, sourceSchema, targetSchema := "ak_bind_"+suffix, "or_bind_"+suffix, "or_dest_"+suffix
	t.Cleanup(func() {
		for _, schema := range []string{authSchema, sourceSchema, sourceSchema + "_jobs", targetSchema, targetSchema + "_jobs"} {
			_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		}
	})
	tenant := iam.NewTenantID()
	other := iam.NewTenantID()
	roles := native.NewRoles(native.APIKeys)
	merchant := roles.Persona(billing.MerchantGroupPersona, native.APIKeys)
	var readerPermission iam.Perm
	for _, permission := range billing.Permissions() {
		if strings.HasPrefix(string(permission), "merchant:") {
			merchant.Declare(string(permission))
		}
		if strings.HasPrefix(string(permission), "root:") {
			declared := roles.Root.Declare(string(permission))[0]
			if permission == billing.RootCustomerRead {
				readerPermission = declared
			}
		}
	}
	readerRole := roles.Root.Role("billing-reader", readerPermission)
	app := roles.Persona("app")
	ak, _ := authtest.New(t,
		authtest.WithConfig(func(c *native.Config) {
			c.DefaultTenantID, c.Database.Schema, c.Database.RiverSchema, c.Roles = tenant, authSchema, authSchema, roles
		}),
		authtest.WithDeps(func(d *native.Deps) { d.Postgres = pool }),
	)
	require.NoError(t, ak.CreateTenant(ctx, iam.Tenant{ID: other}))
	database := openrails.DatabaseConfig{Schema: sourceSchema, RiverSchema: sourceSchema + "_jobs"}
	migrate := func(database openrails.DatabaseConfig) {
		t.Helper()
		require.NoError(t, openrails.Migrate(ctx, pool, openrails.Config{Database: database, ProviderWriteMode: openrails.ProviderWritesReadOnly}))
	}
	migrate(database)
	seed := func(schema, slug string) billing.MerchantID {
		t.Helper()
		id := billing.MerchantID(uuid.New())
		_, err := pool.Exec(ctx, `INSERT INTO `+schema+`.merchants(id,slug,status) VALUES($1,$2,'active')`, id.UUID(), slug)
		require.NoError(t, err)
		return id
	}
	mid := seed(sourceSchema, "host-one")
	binding := billing.MerchantBinding{ID: mid, TenantID: tenant.String()}
	customer := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO `+sourceSchema+`.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), customer)
	require.NoError(t, err)
	counts := func() (int, int) {
		t.Helper()
		var members, keys int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+authSchema+`.group_user_roles`).Scan(&members))
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+authSchema+`.api_keys`).Scan(&keys))
		return members, keys
	}
	beforeMembers, beforeKeys := counts()
	readBinding := func(schema string, id billing.MerchantID) (string, string, time.Time) {
		t.Helper()
		var group, owner *string
		var changed time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT permission_group_id,tenant_id::text,updated_at FROM `+schema+`.merchants WHERE id=$1`, id.UUID()).Scan(&group, &owner, &changed))
		if group == nil {
			group = new(string)
		}
		if owner == nil {
			owner = new(string)
		}
		return *group, *owner, changed
	}

	t.Run("atomic idempotent adoption without grants", func(t *testing.T) {
		require.NoError(t, adapter.BindMerchant(ctx, pool, database, ak, binding))
		group, owner, changed := readBinding(sourceSchema, mid)
		require.Equal(t, mid.String(), group)
		require.Equal(t, tenant.String(), owner)
		metadata, err := adapter.MerchantGroups(ak).LookupMerchantGroup(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, billing.MerchantGroup{ID: mid.String(), TenantID: tenant.String(), Persona: billing.MerchantGroupPersona}, metadata)
		require.NoError(t, adapter.BindMerchant(ctx, pool, database, ak, binding))
		_, _, unchanged := readBinding(sourceSchema, mid)
		require.Equal(t, changed, unchanged)
		members, keys := counts()
		require.Equal(t, beforeMembers, members)
		require.Equal(t, beforeKeys, keys)
		var preserved uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM `+sourceSchema+`.customers WHERE merchant_id=$1 AND id=$2`, mid.UUID(), customer).Scan(&preserved))
		require.Equal(t, customer, preserved)
	})

	t.Run("foreign tenant and existing group refuse without partial binding", func(t *testing.T) {
		foreign := binding
		foreign.TenantID = other.String()
		require.Error(t, adapter.BindMerchant(ctx, pool, database, ak, foreign))
		wrong := seed(sourceSchema, "host-two")
		_, err := ak.CreateGroup(ctx, iam.NewGroup{ID: wrong.String(), TenantID: tenant, Persona: app.Persona})
		require.NoError(t, err)
		require.Error(t, adapter.BindMerchant(ctx, pool, database, ak, billing.MerchantBinding{ID: wrong, TenantID: tenant.String()}))
		group, owner, _ := readBinding(sourceSchema, wrong)
		require.Empty(t, group)
		require.Empty(t, owner)
	})

	t.Run("one merchant per tenant rolls back newly created group", func(t *testing.T) {
		second := seed(sourceSchema, "host-three")
		require.Error(t, adapter.BindMerchant(ctx, pool, database, ak, billing.MerchantBinding{ID: second, TenantID: tenant.String()}))
		_, err := ak.Group(ctx, iam.GroupByID(second.String()))
		require.ErrorIs(t, err, iam.ErrGroupNotFound)
		group, owner, _ := readBinding(sourceSchema, second)
		require.Empty(t, group)
		require.Empty(t, owner)
	})

	t.Run("missing merchant creates no group", func(t *testing.T) {
		missing := billing.MerchantID(uuid.New())
		require.Error(t, adapter.BindMerchant(ctx, pool, database, ak, billing.MerchantBinding{ID: missing, TenantID: tenant.String()}))
		_, err := ak.Group(ctx, iam.GroupByID(missing.String()))
		require.ErrorIs(t, err, iam.ErrGroupNotFound)
	})

	t.Run("database refuses a tenant without a merchant group", func(t *testing.T) {
		id := seed(sourceSchema, "host-invalid")
		_, err := pool.Exec(ctx, `UPDATE `+sourceSchema+`.merchants SET tenant_id=$2 WHERE id=$1`, id.UUID(), other.String())
		require.ErrorContains(t, err, "merchants_tenant_group_check")
		group, owner, _ := readBinding(sourceSchema, id)
		require.Empty(t, group)
		require.Empty(t, owner)
	})

	t.Run("foreign billing binding is never reassigned", func(t *testing.T) {
		id := seed(sourceSchema, "host-four")
		foreignGroup := uuid.NewString()
		_, err := pool.Exec(ctx, `UPDATE `+sourceSchema+`.merchants SET permission_group_id=$2 WHERE id=$1`, id.UUID(), foreignGroup)
		require.NoError(t, err)
		require.Error(t, adapter.BindMerchant(ctx, pool, database, ak, billing.MerchantBinding{ID: id, TenantID: tenant.String()}))
		group, owner, _ := readBinding(sourceSchema, id)
		require.Equal(t, foreignGroup, group)
		require.Empty(t, owner)
		_, err = ak.Group(ctx, iam.GroupByID(id.String()))
		require.ErrorIs(t, err, iam.ErrGroupNotFound)
	})

	var providerWrites atomic.Int32
	transport := roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			providerWrites.Add(1)
		}
		return nil, errors.New("fixture has no external providers")
	})
	open := func(database openrails.DatabaseConfig, binding *billing.MerchantBinding) *openrails.Engine {
		t.Helper()
		database.SkipMigrations = true
		engine, err := openrails.New(ctx, openrails.Config{
			Database: database, ProviderWriteMode: openrails.ProviderWritesReadOnly,
			Merchants: map[string]openrails.MerchantDeclaration{"host-one": {Binding: binding, Issuer: authtest.Issuer}},
		}, openrails.Deps{Postgres: pool, MerchantGroups: adapter.MerchantGroups(ak), StripeTransport: transport, NMITransport: transport, FXTransport: transport})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
		return engine
	}
	t.Run("default declaration refuses bound identity", func(t *testing.T) {
		_, err := open(database, nil).MerchantBySlug("host-one")
		require.Error(t, err)
	})
	var source *openrails.Client
	var sourceEngine *openrails.Engine
	t.Run("explicit matching declaration survives restart", func(t *testing.T) {
		var err error
		sourceEngine = open(database, &binding)
		source, err = sourceEngine.MerchantBySlug("host-one")
		require.NoError(t, err)
		require.Equal(t, mid, source.MerchantID())
	})
	t.Run("explicit foreign tenant declaration is refused", func(t *testing.T) {
		foreign := binding
		foreign.TenantID = other.String()
		_, err := open(database, &foreign).MerchantBySlug("host-one")
		require.Error(t, err)
	})
	t.Run("native signed users require the merchant tenant while staff stay separate", func(t *testing.T) {
		member := authtest.NewUser(t, ak)
		foreignView, err := ak.ForTenant(ctx, other)
		require.NoError(t, err)
		operator := authtest.NewUser(t, foreignView)
		authtest.GrantRole(t, foreignView, iam.RootGroup(), iam.UserSubject(operator.ID), readerRole)
		beforeMembers++ // explicit fixture setup, not the binding operation
		memberToken := authtest.SignIn(t, ak, member)
		operatorToken := authtest.SignIn(t, foreignView, operator)
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, sourceEngine, openrails.Routes{Auth: ak.Authenticator(), RouteGroups: openrails.RouteGroups{Admin: true}}))
		request := func(path, token string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			out := httptest.NewRecorder()
			mux.ServeHTTP(out, req)
			return out
		}
		good := request("/v1/me", memberToken.AccessToken)
		require.Equal(t, http.StatusOK, good.Code, good.Body.String())
		denied := request("/v1/me", operatorToken.AccessToken)
		require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
		require.Contains(t, denied.Body.String(), billing.CodeCustomerTenantMismatch)
		staff := request("/v1/admin/access", operatorToken.AccessToken)
		require.Equal(t, http.StatusOK, staff.Code, staff.Body.String())
		var phantom int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+sourceSchema+`.customers WHERE merchant_id=$1 AND id=$2`, mid.UUID(), operator.ID).Scan(&phantom))
		require.Zero(t, phantom, "staff authority must not create customer ownership")
	})

	t.Run("external subjects keep issuer ownership even in the same tenant", func(t *testing.T) {
		// This custom test authenticator models a verified external credential.
		// Its group tenant is not proof that its subject is a native account.
		foreign := &billingauthtest.Fake{}
		token := func(id uuid.UUID) string {
			identity := billingauthtest.User(id.String())
			identity.Issuer, identity.Invoker.Issuer = "https://external.example", "https://external.example"
			identity.TenantID = tenant.String()
			return foreign.Issue(billingauthtest.Grant{Identity: identity, Bound: billingauthtest.MerchantScope(mid)})
		}
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, sourceEngine, openrails.Routes{Auth: foreign}))
		request := func(id uuid.UUID) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			req.Header.Set("Authorization", "Bearer "+token(id))
			out := httptest.NewRecorder()
			mux.ServeHTTP(out, req)
			return out
		}
		denied := request(customer)
		require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
		require.Contains(t, denied.Body.String(), billing.CodeCustomerIssuerMismatch)
		var issuer *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT issuer FROM `+sourceSchema+`.customers WHERE merchant_id=$1 AND id=$2`, mid.UUID(), customer).Scan(&issuer))
		require.Nil(t, issuer, "external credentials cannot claim the native customer")
		externalID := uuid.New()
		allowed := request(externalID)
		require.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
		require.NoError(t, pool.QueryRow(ctx, `SELECT issuer FROM `+sourceSchema+`.customers WHERE merchant_id=$1 AND id=$2`, mid.UUID(), externalID).Scan(&issuer))
		require.NotNil(t, issuer)
		require.Equal(t, "https://external.example", *issuer)
	})

	t.Run("archive carries tenant and preserves destination ownership", func(t *testing.T) {
		require.NotNil(t, source)
		var archive bytes.Buffer
		require.NoError(t, source.ExportBillingArchive(ctx, &archive))
		header, _, err := archivewire.ReadHeader(bytes.NewReader(archive.Bytes()))
		require.NoError(t, err)
		require.Equal(t, tenant.String(), header.TenantID)
		destinationDB := openrails.DatabaseConfig{Schema: targetSchema, RiverSchema: targetSchema + "_jobs"}
		migrate(destinationDB)
		_, err = pool.Exec(ctx, `INSERT INTO `+targetSchema+`.merchants(id,slug,status) VALUES($1,'host-one','active')`, mid.UUID())
		require.NoError(t, err)
		require.NoError(t, adapter.BindMerchant(ctx, pool, destinationDB, ak, binding))
		destination, err := open(destinationDB, &binding).MerchantBySlug("host-one")
		require.NoError(t, err)
		_, err = destination.ImportBillingArchive(ctx, bytes.NewReader(archive.Bytes()))
		require.NoError(t, err)
		var restored uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM `+targetSchema+`.customers WHERE merchant_id=$1 AND id=$2`, mid.UUID(), customer).Scan(&restored))
		require.Equal(t, customer, restored)
		group, owner, _ := readBinding(targetSchema, mid)
		require.Equal(t, mid.String(), group)
		require.Equal(t, tenant.String(), owner)
		wrongOwner := bytes.Replace(archive.Bytes(), []byte(tenant.String()), []byte(other.String()), 1)
		_, err = destination.ImportBillingArchive(ctx, bytes.NewReader(wrongOwner))
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, "billing_archive_merchant_mismatch", refusal.Code)
		group, owner, _ = readBinding(targetSchema, mid)
		require.Equal(t, mid.String(), group)
		require.Equal(t, tenant.String(), owner)
	})
	require.Zero(t, providerWrites.Load())
	members, keys := counts()
	require.Equal(t, beforeMembers, members)
	require.Equal(t, beforeKeys, keys)
}
