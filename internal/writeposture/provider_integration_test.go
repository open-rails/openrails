//go:build integration

package writeposture_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/writeposture"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderWritePostureAtDispatch(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_TEST_PROVIDER_POSTURE_DSN")
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_PROVIDER_POSTURE_DSN to a disposable PostgreSQL database")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "psp_posture_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := pool.Exec(cleanup, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema}))
	database, err := db.NewWithPGXPool(pool, schema)
	require.NoError(t, err)
	q := database.GenDirectory()
	mid, err := db.RegisterUnboundMerchant(ctx, database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: "alpha"})
	require.NoError(t, err)
	other, err := db.RegisterUnboundMerchant(ctx, database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: "beta"})
	require.NoError(t, err)
	declare := func(owner billing.MerchantID, rail, key, externalID string) uuid.UUID {
		t.Helper()
		row, err := q.DeclarePSPIdentity(ctx, gen.DeclarePSPIdentityParams{
			ID: uuid.New(), MerchantID: owner.UUID(), Rail: rail, Environment: "test", Key: key, AccountID: externalID,
		})
		require.NoError(t, err)
		require.Equal(t, "readonly", row.WriteMode)
		return row.ID
	}
	stripeHeld := declare(mid, "stripe", "primary", "acct_primary")
	stripeSibling := declare(mid, "stripe", "sibling", "acct_sibling")
	stripeOther := declare(other, "stripe", "other", "acct_other")
	nmiID := declare(mid, "nmi", "gateway", "rehearsal_gateway")
	set := func(owner billing.MerchantID, id uuid.UUID, mode string) {
		t.Helper()
		changed, err := q.SetPSPWriteMode(ctx, gen.SetPSPWriteModeParams{MerchantID: owner.UUID(), PspID: id, WriteMode: mode})
		require.NoError(t, err)
		require.EqualValues(t, 1, changed)
	}
	set(mid, stripeSibling, "full")
	set(other, stripeOther, "full")
	cfg := &config.Config{ProviderWriteMode: "full"}
	var stripeWrites, stripeReads atomic.Int32
	stripe := stripeapi.NewFactory(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			stripeReads.Add(1)
		} else {
			stripeWrites.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}), database)
	stripeRequest := func(client *http.Client, id uuid.UUID, origin writeposture.Origin, method string) error {
		t.Helper()
		requestCtx := writeposture.WithOrigin(db.WithPSPID(merchant.WithID(ctx, mid), id), origin)
		req, err := http.NewRequestWithContext(requestCtx, method, "https://api.stripe.com/v1/payment_intents", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer sk_test_fixture")
		response, err := client.Do(req)
		if response != nil {
			response.Body.Close()
		}
		return err
	}
	client := stripe.Client(cfg, 0)
	require.ErrorIs(t, stripeRequest(client, stripeHeld, writeposture.OriginUser, http.MethodPost), stripeapi.ErrProviderReadOnly)
	require.ErrorIs(t, stripeRequest(client, stripeOther, writeposture.OriginUser, http.MethodPost), stripeapi.ErrProviderReadOnly)
	require.ErrorIs(t, stripeRequest(client, uuid.Nil, writeposture.OriginUser, http.MethodPost), stripeapi.ErrProviderReadOnly)
	require.NoError(t, stripeRequest(client, stripeHeld, writeposture.OriginSystem, http.MethodGet))
	require.EqualValues(t, 0, stripeWrites.Load())
	require.EqualValues(t, 1, stripeReads.Load())
	require.NoError(t, stripeRequest(client, stripeSibling, writeposture.OriginSystem, http.MethodPost))
	require.ErrorIs(t, stripeRequest(stripe.Client(&config.Config{ProviderWriteMode: "limited"}, 0), stripeSibling, writeposture.OriginSystem, http.MethodPost), stripeapi.ErrProviderReadOnly)
	set(mid, stripeSibling, "limited")
	require.ErrorIs(t, stripeRequest(client, stripeSibling, writeposture.OriginSystem, http.MethodPost), stripeapi.ErrProviderReadOnly)
	for _, origin := range []writeposture.Origin{writeposture.OriginUser, writeposture.OriginAdmin, writeposture.OriginProvider} {
		require.NoError(t, stripeRequest(client, stripeSibling, origin, http.MethodPost))
	}
	require.EqualValues(t, 4, stripeWrites.Load())

	var nmiWrites atomic.Int32
	nmiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			nmiWrites.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(nmiServer.Close)
	nmiCfg := &config.Config{ProviderWriteMode: "full", ProviderSandbox: &config.ProviderSandboxConfig{NMIGatewayURL: nmiServer.URL}}
	nmiClient, err := (&railresolve.NMIFactory{Config: nmiCfg, DB: database}).ClientFor(mid, merchants.PSPScope{
		ID: nmiID, Rail: "nmi", Environment: "test", AccountID: "rehearsal_gateway",
	}, &config.NMIProviderSettings{SecurityKey: "fixture", Environment: "test"})
	require.NoError(t, err)
	mutationCtx := writeposture.WithOrigin(merchant.WithID(ctx, mid), writeposture.OriginAdmin)
	deleteAtNMI := func() error {
		return nmiClient.DeleteCustomerVault(mutationCtx, nmi.DeleteCustomerVaultData{CustomerVaultID: "fixture"})
	}
	require.ErrorIs(t, deleteAtNMI(), nmi.ErrProviderReadOnly)
	set(mid, nmiID, "limited")
	require.NoError(t, deleteAtNMI())
	require.NoError(t, writeposture.Set(ctx, q, mid.UUID(), writeposture.ReadOnly, writeposture.ReasonOperator, "fixture", time.Now()))
	require.ErrorIs(t, deleteAtNMI(), nmi.ErrProviderReadOnly)
	require.ErrorIs(t, stripeRequest(client, stripeSibling, writeposture.OriginUser, http.MethodPost), stripeapi.ErrProviderReadOnly)
	require.NoError(t, q.DeleteWritePosture(ctx, mid.UUID()))
	_, err = pool.Exec(ctx, "UPDATE "+pgx.Identifier{schema, "book_identity"}.Sanitize()+" SET schema_oid = 0")
	require.NoError(t, err)
	require.ErrorIs(t, deleteAtNMI(), nmi.ErrProviderReadOnly)
	require.ErrorIs(t, stripeRequest(client, stripeSibling, writeposture.OriginUser, http.MethodPost), stripeapi.ErrProviderReadOnly)
	require.EqualValues(t, 1, nmiWrites.Load())
	require.EqualValues(t, 4, stripeWrites.Load())
}
