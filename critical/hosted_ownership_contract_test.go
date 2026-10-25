//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/billing"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/stretchr/testify/require"
)

func TestHostedOwnershipContracts(t *testing.T) {
	t.Run("OR-J31-S05/guarded-hosted-state-removal-preserves-progress", hostedStateRemoval)
	t.Run("OR-J01-S16/host-admission-preserves-proof-and-billing-scope", hostedCustomerAdmission)
}

func extractionOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func extractionDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("OPENRAILS_CRITICAL_DSN")
	if raw == "" {
		t.Fatal("OPENRAILS_CRITICAL_DSN must name an owned disposable PostgreSQL server")
	}
	admin, err := pgxpool.New(t.Context(), raw)
	extractionOK(t, err)
	name := "or_extract_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	extractionOK(t, err)
	uri, err := url.Parse(raw)
	extractionOK(t, err)
	uri.Path = "/" + name
	pool, err := pgxpool.New(t.Context(), uri.String())
	extractionOK(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize())
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	return pool
}

func hostedStateRemoval(t *testing.T) {
	pool := extractionDatabase(t)
	chain, err := migratekit.LoadFromFS(postgresmigrations.FS)
	extractionOK(t, err)
	var before []migratekit.Migration
	var removal migratekit.Migration
	for _, m := range chain {
		if migratekit.Prefix(m.Name) == "116" {
			removal = m
		} else {
			before = append(before, m)
		}
	}
	if removal.Name == "" {
		t.Fatal("missing removal migration")
	}
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	extractionOK(t, err)
	defer migrator.Close()
	extractionOK(t, migrator.WithSchema("billing").ApplyMigrations(t.Context(), before))
	for _, tc := range []struct{ name, setup string }{
		{"cursor state is not empty data", `INSERT INTO billing.network_event_cursors(issuer,tenant_id,cursor) VALUES('https://accounts.invalid','00000000-0000-4000-8000-000000000001','')`},
		{"partial legacy pair", `ALTER TABLE billing.customer_links RENAME TO saved_customer_links`},
		{"conflicting destination", `CREATE SCHEMA saas; CREATE TABLE saas.customer_links(id uuid); CREATE TABLE saas.network_event_cursors(cursor text)`},
		{"missing pair is not presumed empty", `DROP TABLE billing.customer_links; DROP TABLE billing.network_event_cursors`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			extractionOK(t, err)
			defer tx.Rollback(context.Background())
			_, err = tx.Exec(t.Context(), tc.setup)
			extractionOK(t, err)
			_, err = tx.Exec(t.Context(), removal.Content)
			if err == nil {
				t.Fatal("unsafe state accepted")
			}
			if !strings.Contains(err.Error(), "transfer") {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
	// A second transaction holding a source table must be refused without waiting.
	t.Run("busy source", func(t *testing.T) {
		hold, err := pool.Begin(t.Context())
		extractionOK(t, err)
		defer hold.Rollback(context.Background())
		_, err = hold.Exec(t.Context(), "LOCK TABLE billing.network_event_cursors IN ACCESS SHARE MODE")
		extractionOK(t, err)
		tx, err := pool.Begin(t.Context())
		extractionOK(t, err)
		defer tx.Rollback(context.Background())
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err = tx.Exec(ctx, removal.Content)
		if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "could not obtain lock") {
			t.Fatalf("expected immediate lock refusal, got %v", err)
		}
	})
	// A complete explicit schema move preserves nonempty opaque reader state.
	t.Run("transferred cursor preserved", func(t *testing.T) {
		tx, err := pool.Begin(t.Context())
		extractionOK(t, err)
		defer tx.Rollback(context.Background())
		_, err = tx.Exec(t.Context(), `INSERT INTO billing.network_event_cursors(issuer,tenant_id,cursor) VALUES('https://accounts.invalid','00000000-0000-4000-8000-000000000001','opaque-bound-filter'); CREATE SCHEMA saas; ALTER TABLE billing.customer_links SET SCHEMA saas; ALTER TABLE billing.network_event_cursors SET SCHEMA saas`)
		extractionOK(t, err)
		_, err = tx.Exec(t.Context(), removal.Content)
		extractionOK(t, err)
		var cursor string
		err = tx.QueryRow(t.Context(), "SELECT cursor FROM saas.network_event_cursors").Scan(&cursor)
		extractionOK(t, err)
		if cursor != "opaque-bound-filter" {
			t.Fatal(cursor)
		}
	})
	extractionOK(t, migrator.WithSchema("billing").ApplyMigrations(t.Context(), chain))
	extractionOK(t, migrator.WithSchema("billing").ApplyMigrations(t.Context(), chain))
	var absent bool
	extractionOK(t, pool.QueryRow(t.Context(), "SELECT to_regclass('billing.customer_links') IS NULL AND to_regclass('billing.network_event_cursors') IS NULL").Scan(&absent))
	if !absent {
		t.Fatal("fresh core retained hosted tables")
	}
}

func hostedCustomerAdmission(t *testing.T) {
	w := newStandaloneWorld(t, func(cfg map[string]any) { cfg["CustomerAdmissionPolicy"] = true; cfg["MappedCustomers"] = true })
	var native struct {
		Value string `json:"value"`
	}
	w.command(t, map[string]string{"Op": "native-customer-token", "Merchant": "alpha", "Account": "alice"}, &native)
	require.NotEmpty(t, native.Value)
	customer := billing.CustomerID(uuid.MustParse(w.AliceID))
	status, raw := w.request(t, http.MethodGet, "/v1/me/orders", native.Value, nil)
	require.Equal(t, http.StatusForbidden, status, string(raw))
	_, err := w.alpha.GetCustomer(t.Context(), customer)
	require.ErrorIs(t, err, billing.ErrNotFound, "policy refuses before customer allocation")
	var policy map[string]bool
	w.command(t, map[string]string{"Op": "customer-policy", "ID": "allow"}, &policy)
	status, raw = w.request(t, http.MethodGet, "/v1/me/orders", native.Value, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	w.command(t, map[string]string{"Op": "customer-policy", "ID": "deny"}, &policy)
	status, raw = w.request(t, http.MethodGet, "/v1/me/orders", native.Value, nil)
	require.Equal(t, http.StatusForbidden, status, string(raw), "existing row does not bypass host policy")
	_, err = w.alpha.GetCustomer(t.Context(), customer)
	require.NoError(t, err, "staff reads keep their own permission gate")
	// Merchant-asserted identities keep ordinary merchant-specific access.
	status, raw = w.request(t, http.MethodGet, "/v1/me/orders", w.Merchants["alpha"].CustomerToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = w.request(t, http.MethodGet, "/__mapped/alpha/me/orders", w.AliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw), "host mapping is separate from automatic admission")
	for _, entry := range []struct{ method, path string }{{http.MethodGet, "/v1/network/links"}, {http.MethodPost, "/v1/me/network-link/challenges"}, {http.MethodGet, "/pay/sign-in"}} {
		status, raw = w.request(t, entry.method, entry.path, native.Value, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
	}
	w.command(t, map[string]string{"Op": "grant-customer-owner", "Merchant": "alpha", "Account": "alice"}, &policy)
	var token struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	w.command(t, map[string]any{"Op": "authorize", "Merchant": "alpha", "Account": "alice", "Scopes": []string{"openid", "fixture:host", billing.ScopeSelf}}, &token)
	require.Contains(t, strings.Fields(token.Scope), "fixture:host")
	w.command(t, map[string]string{"Op": "customer-policy", "ID": "allow"}, &policy)
	status, raw = w.request(t, http.MethodGet, "/v1/me/orders", token.AccessToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = w.request(t, http.MethodGet, "/v1/admin/customers", token.AccessToken, nil)
	require.Equal(t, http.StatusForbidden, status, string(raw), "host override cannot broaden canonical self scope")
	w.command(t, map[string]string{"Op": "legacy-account-fact", "Merchant": "alpha", "Account": "alice"}, &policy)
	events, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"customer.network_linked"}})
	require.NoError(t, err)
	require.Len(t, events.Events, 1)
	require.Equal(t, "customer.network_linked", events.Events[0].Type)
	var legacy struct {
		Legacy bool `json:"legacy"`
	}
	require.NoError(t, json.Unmarshal(events.Events[0].Data, &legacy))
	require.True(t, legacy.Legacy)
	status, raw = w.request(t, http.MethodGet, "/v1/config", "", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var configuration map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &configuration))
	require.NotContains(t, configuration, "network")
}
