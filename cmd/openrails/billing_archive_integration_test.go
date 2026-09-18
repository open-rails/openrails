//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// The credential fixture sits behind the same merchant permission/binding gate
// as the server; archive handlers and HTTP transport are not mocked.
type archiveCLIKeyResolver struct{ mid merchant.ID }

func (r archiveCLIKeyResolver) LooksLikeAPIKey(string) bool { return true }
func (r archiveCLIKeyResolver) ResolveAPIKey(_ context.Context, token string) (*controlplane.ResolvedServiceCredential, error) {
	if token != "archive-cli-owner" {
		return nil, fmt.Errorf("invalid fixture credential")
	}
	return &controlplane.ResolvedServiceCredential{MerchantID: r.mid, Permissions: []string{"merchant:*"}}, nil
}

func TestBillingArchiveCLILocalHTTPAndBack(t *testing.T) {
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)
	mid := merchant.ID(uuid.New())
	dir := t.TempDir()
	type deployment struct {
		cfg      *config.Config
		path     string
		database *db.DB
	}
	setup := func(schema string) deployment {
		require.NoError(t, migrate.RunPostgres(t.Context(), &config.Config{DB: &config.DBConfig{URL: superDSN, Schema: schema}}))
		cfg := &config.Config{DB: &config.DBConfig{URL: appDSN, Schema: schema}}
		d, err := openCLIDB(t.Context(), cfg)
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		path := filepath.Join(dir, schema+".yaml")
		// No ENV, provider, Vault, Redis or AuthKit configuration is supplied.
		require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("db:\n  url: %q\n  schema: %s\n", appDSN, schema)), 0600))
		return deployment{cfg, path, d}
	}
	source, target, back := setup("cli_archive_source"), setup("cli_archive_target"), setup("cli_archive_back")
	run := func(args ...string) string {
		t.Helper()
		cmd := newRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetContext(t.Context())
		cmd.SetArgs(args)
		require.NoError(t, cmd.Execute(), out.String())
		return out.String()
	}
	for _, d := range []deployment{source, target, back} {
		run("--config", d.path, "billing", "prepare-target", "--merchant", mid.String(), "--unbound-merchants", "--slug", "archive-cli")
	}
	ctx := merchant.WithID(t.Context(), mid)
	require.NoError(t, source.database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO openrails.customers(merchant_id,id,issuer) SELECT $1,gen_random_uuid(),'https://identity.example' FROM generate_series(1,7000)`, mid.UUID())
		return err
	}))
	workerSnapshot := func(d deployment) string {
		var snapshot string
		require.NoError(t, d.database.Qx(t.Context()).QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(w) ORDER BY to_jsonb(w)::text),'[]'::jsonb)::text FROM openrails.worker_state w`).Scan(&snapshot))
		return snapshot
	}
	before := workerSnapshot(source)
	original := filepath.Join(dir, "original.ndjson")
	run("--config", source.path, "billing", "export", "--source-stopped", "--merchant", mid.String(), "--out", original)
	require.Equal(t, before, workerSnapshot(source), "source CLI must not initialize worker progress state")
	raw, err := os.ReadFile(original)
	require.NoError(t, err)
	require.Greater(t, len(raw), 1<<20)
	rt := &app.Runtime{DB: target.database, Config: target.cfg, Clock: clockwork.NewRealClock()}
	mux := http.NewServeMux()
	opts := httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{ServiceCredentialResolver: archiveCLIKeyResolver{mid}})}
	httproutes.RegisterMerchantArchiveRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, opts)
	server := httptest.NewServer(middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes)(mux))
	defer server.Close()
	token := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(token, []byte("archive-cli-owner"), 0600))
	remote := []string{"--merchant", mid.String(), "--url", server.URL, "--token-file", token}
	run(append([]string{"billing", "import", "--in", original}, remote...)...)
	require.Contains(t, run(append([]string{"billing", "import", "--in", original}, remote...)...), "already_imported=true")
	second := filepath.Join(dir, "second.ndjson")
	run(append([]string{"billing", "export", "--source-stopped", "--out", second}, remote...)...)
	got, err := os.ReadFile(second)
	require.NoError(t, err)
	require.Equal(t, raw, got)
	run("--config", back.path, "billing", "import", "--merchant", mid.String(), "--in", second)
	third := filepath.Join(dir, "third.ndjson")
	run("--config", back.path, "billing", "export", "--source-stopped", "--merchant", mid.String(), "--out", third)
	got, err = os.ReadFile(third)
	require.NoError(t, err)
	require.Equal(t, raw, got)
	require.Equal(t, before, workerSnapshot(source))
}
