//go:build e2e && integration

package ci_test

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/network"
	"github.com/open-rails/openrails/server/internal/operator"
	"github.com/stretchr/testify/require"
)

func pollNetworkEvents(t *testing.T, srv *server.Server) (int, error) {
	t.Helper()
	graph, _ := operator.Of(srv)
	return graph.Runtime.Network.(*network.Service).PollEvents(t.Context())
}

// A source event is not acknowledged. Only this server's projection and cursor
// commit, atomically; replay after a failed local commit cannot duplicate work.
func TestNetworkEventCursorCommitsWithProjection(t *testing.T) {
	// The full server keeps a dedicated verifier LISTEN connection. Give
	// this reader its own single-connection billing handle while AuthKit
	// retains its separate normal pool.
	w := newWorld(t)
	graph, _ := operator.Of(w.srv)
	cfg := graph.Runtime.DB.Pool().Config()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(single.Close)
	database, err := db.NewWithPGXPool(single, graph.Config.Database.Schema)
	require.NoError(t, err)
	reader, err := network.New(database, w.srv.AuthKit(), abusestate.New(nil), nil, w.tenant, w.as.URL)
	require.NoError(t, err)
	poll := func() (int, error) { return reader.PollEvents(t.Context()) }
	ctx := t.Context()
	account := w.account(true)
	sub := uuid.NewString()
	original := w.connect(w.customer(w.shopLogin, sub, nil), w.shopClient, account)
	_, err = poll()
	require.NoError(t, err)
	pool := graph.Runtime.DB.DataPool()
	var beforeRevocation string
	require.NoError(t, pool.QueryRow(ctx, `SELECT cursor FROM billing.network_event_cursors WHERE issuer=$1 AND tenant_id=$2`, w.as.URL, uuid.UUID(w.tenant)).Scan(&beforeRevocation))
	_, err = pool.Exec(ctx, `CREATE FUNCTION billing.refuse_network_cursor() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected cursor failure'; END $$;
CREATE TRIGGER refuse_network_cursor BEFORE UPDATE ON billing.network_event_cursors FOR EACH ROW EXECUTE FUNCTION billing.refuse_network_cursor()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `DROP TRIGGER IF EXISTS refuse_network_cursor ON billing.network_event_cursors; DROP FUNCTION IF EXISTS billing.refuse_network_cursor()`)
	})
	session := "Bearer " + authtest.SignIn(t, w.srv.AuthKit(), account).AccessToken
	r := serve(w.handler, rsRequest{method: http.MethodDelete, path: w.srv.AuthKit().APIBase() + "/me/oauth-consents/" + url.PathEscape(w.shopClient.ClientID), authorization: session})
	require.Equal(t, http.StatusNoContent, r.Code, r.Body.String())
	_, err = poll()
	require.ErrorContains(t, err, "injected cursor failure")
	customer, _ := w.adminCustomer(w.shopKey, sub)
	require.NotNil(t, customer.Network, "projection rolls back with cursor failure")
	_, err = pool.Exec(ctx, `DROP TRIGGER refuse_network_cursor ON billing.network_event_cursors; DROP FUNCTION billing.refuse_network_cursor()`)
	require.NoError(t, err)
	_, err = poll()
	require.NoError(t, err)
	customer, _ = w.adminCustomer(w.shopKey, sub)
	require.Nil(t, customer.Network)
	count, err := poll()
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, 1, w.unlinkedEvents(w.shopKey, sub))

	// A later explicit re-link is a new relationship. Restoring/replaying an
	// older reader cursor must not apply its earlier revocation to this row.
	relinked := w.connect(w.customer(w.shopLogin, sub, nil), w.shopClient, account)
	require.NotEqual(t, original.ID, relinked.ID)
	require.True(t, relinked.LinkedAt.After(original.LinkedAt))
	_, err = pool.Exec(ctx, `UPDATE billing.network_event_cursors SET cursor=$1 WHERE issuer=$2 AND tenant_id=$3`, beforeRevocation, w.as.URL, uuid.UUID(w.tenant))
	require.NoError(t, err)
	_, err = poll()
	require.NoError(t, err)
	customer, _ = w.adminCustomer(w.shopKey, sub)
	require.NotNil(t, customer.Network, "old revocation cannot erase a newer proved relationship")
	require.Equal(t, 1, w.unlinkedEvents(w.shopKey, sub), "replaying an old revocation emits no new unlink fact")
}
