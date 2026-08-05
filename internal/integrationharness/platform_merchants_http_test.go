//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// #721: platform operator merchant directory — /v1/platform/merchants over the
// REAL standalone stack (real AuthKit root-group grants, real RLS app role).

type platformMerchantJSON struct {
	ID            string     `json:"id"`
	Slug          string     `json:"slug"`
	Status        string     `json:"status"`
	DisplayName   *string    `json:"display_name"`
	CreatedAt     time.Time  `json:"created_at"`
	DeletedAt     *time.Time `json:"deleted_at"`
	RailsArmed    []string   `json:"rails_armed"`
	LastPaymentAt *time.Time `json:"last_payment_at"`
}

type platformMerchantListJSON struct {
	Data    []platformMerchantJSON `json:"data"`
	Total   int64                  `json:"total"`
	Limit   int                    `json:"limit"`
	Offset  int                    `json:"offset"`
	HasMore bool                   `json:"has_more"`
}

// mintPlatformOperatorToken creates a real AuthKit user, grants it the given
// ROOT-group role (#721 merchant-directory-*), and mints a user access token.
func mintPlatformOperatorToken(t *testing.T, ctx context.Context, s *Surface, role string) string {
	t.Helper()
	cp := embcp.Get(s.app)
	require.NotNil(t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(t, core, "authkit core")

	username := "platop" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	user, err := core.CreateUser(ctx, username+"@example.com", username)
	require.NoError(t, err, "create platform operator user")
	if role != "" {
		_, err = core.EnsureRootGroup(ctx)
		require.NoError(t, err, "ensure root group")
		require.NoError(t,
			core.Genesis().AssignGroupRole(ctx, authcore.RootPersona, "", user.ID, authcore.SubjectKindUser, role),
			"assign root role %s", role)
	}
	token, _, err := core.MintAccessToken(ctx, user.ID, nil)
	require.NoError(t, err, "issue access token")
	return token
}

func platformListMerchants(t *testing.T, baseURL, token, query string) (int, platformMerchantListJSON, []byte) {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet, baseURL+"/v1/platform/merchants"+query, token, nil)
	var out platformMerchantListJSON
	if status == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &out), string(body))
	}
	return status, out, body
}

func findPlatformMerchant(items []platformMerchantJSON, slug string) (platformMerchantJSON, int) {
	for i, it := range items {
		if it.Slug == slug {
			return it, i
		}
	}
	return platformMerchantJSON{}, -1
}

func TestPlatformMerchantDirectoryListHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	opToken := mintPlatformOperatorToken(t, ctx, surface, controlplane.RootRoleMerchantDirectoryAdmin)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	slugA := "plat-a-" + suffix
	slugB := "plat-b-" + suffix
	slugC := "plat-c-" + suffix
	a := surface.ProvisionOwnedMerchant(slugA)
	b := surface.ProvisionOwnedMerchant(slugB)
	c := surface.ProvisionOwnedMerchant(slugC)
	t.Cleanup(func() {
		for _, m := range []OwnedMerchant{a, b, c} {
			_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.psps WHERE merchant_id = $1", m.MerchantID.UUID())
			_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.payments WHERE merchant_id = $1", m.MerchantID.UUID())
			_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE merchant_id = $1", m.MerchantID.UUID())
			_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE merchant_id = $1", m.MerchantID.UUID())
			_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.customers WHERE merchant_id = $1", m.MerchantID.UUID())
			_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.merchants WHERE id = $1", m.MerchantID.UUID())
		}
	})

	// Rails-armed fixture for B: two live rails + one archived (must be excluded).
	// Seeded over the super pool (RLS bypass) — the SERVER reads them back under
	// the app role via GUC-pinned per-merchant probes.
	for _, row := range []struct{ rail, account string }{
		{"nmi", "gw-" + suffix},
		{"stripe", "acct_" + suffix},
	} {
		_, err := h.Pool().Exec(ctx, `
			INSERT INTO openrails.psps (merchant_id, rail, environment, account_id)
			VALUES ($1, $2, 'test', $3)`, b.MerchantID.UUID(), row.rail, row.account)
		require.NoError(t, err)
	}
	_, err := h.Pool().Exec(ctx, `
		INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived)
		VALUES ($1, 'solana', 'test', $2, true)`, b.MerchantID.UUID(), "wallet-"+suffix)
	require.NoError(t, err)

	// Last-activity fixture for A: one payment (product -> price -> customer -> payment).
	productID, priceID, customerID := uuid.New(), uuid.New(), uuid.New()
	paidAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	_, err = h.Pool().Exec(ctx, `
		INSERT INTO openrails.products (id, merchant_id, key, display_name)
		VALUES ($1, $2, $3, 'Plat Directory Product')`, productID, a.MerchantID.UUID(), "plat-prod-"+suffix)
	require.NoError(t, err)
	_, err = h.Pool().Exec(ctx, `
		INSERT INTO openrails.prices (id, merchant_id, product_id, amount, currency)
		VALUES ($1, $2, $3, 1000000, 'USD')`, priceID, a.MerchantID.UUID(), productID)
	require.NoError(t, err)
	_, err = h.Pool().Exec(ctx, `
		INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`, customerID, a.MerchantID.UUID())
	require.NoError(t, err)
	aPSP := dbtest.EnsureTestPSP(ctx, t, h.Pool(), a.MerchantID.UUID(), "nmi")
	_, err = h.Pool().Exec(ctx, `
		INSERT INTO openrails.payments (merchant_id, customer_id, price_id, rail, transaction_id, amount, list_amount, currency, created_at, psp_id)
		VALUES ($1, $2, $3, 'nmi', $4, 1000000, 1000000, 'USD', $5, $6)`,
		a.MerchantID.UUID(), customerID, priceID, "txn-"+suffix, paidAt, aPSP)
	require.NoError(t, err)

	// Full list: fields, rails-armed, last-activity, ordering.
	status, list, body := platformListMerchants(t, surface.BaseURL, opToken, "?limit=200")
	require.Equal(t, http.StatusOK, status, string(body))
	require.GreaterOrEqual(t, list.Total, int64(4)) // bootstrap merchant + A/B/C at minimum

	itemA, idxA := findPlatformMerchant(list.Data, slugA)
	itemB, idxB := findPlatformMerchant(list.Data, slugB)
	_, idxC := findPlatformMerchant(list.Data, slugC)
	require.NotEqual(t, -1, idxA, "merchant A in default list")
	require.NotEqual(t, -1, idxB, "merchant B in default list")
	require.NotEqual(t, -1, idxC, "merchant C in default list")

	// created_at DESC, id DESC: C was provisioned last, A first.
	require.Less(t, idxC, idxB, "newest first")
	require.Less(t, idxB, idxA, "newest first")

	require.Equal(t, "active", itemA.Status)
	require.Equal(t, a.MerchantID.String(), itemA.ID)
	require.False(t, itemA.CreatedAt.IsZero())
	require.Nil(t, itemA.DeletedAt)
	require.Equal(t, []string{}, itemA.RailsArmed, "A has no rail accounts")
	require.NotNil(t, itemA.LastPaymentAt, "A has a payment")
	require.WithinDuration(t, paidAt, *itemA.LastPaymentAt, time.Second)

	require.Equal(t, []string{"nmi", "stripe"}, itemB.RailsArmed, "distinct non-archived rails, sorted")
	require.Nil(t, itemB.LastPaymentAt, "B has no payments")

	// Get by id (includes the same enrichment).
	status, body = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/platform/merchants/"+b.MerchantID.String(), opToken, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var got platformMerchantJSON
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, slugB, got.Slug)
	require.Equal(t, "active", got.Status)
	require.Equal(t, []string{"nmi", "stripe"}, got.RailsArmed)

	// Get unknown / invalid ids.
	status, _ = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/platform/merchants/"+uuid.NewString(), opToken, nil)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/platform/merchants/not-a-uuid", opToken, nil)
	require.Equal(t, http.StatusBadRequest, status)

	// Pagination: stable pages, no overlap, envelope bookkeeping.
	status, page1, body := platformListMerchants(t, surface.BaseURL, opToken, "?limit=2&offset=0")
	require.Equal(t, http.StatusOK, status, string(body))
	require.Len(t, page1.Data, 2)
	require.Equal(t, 2, page1.Limit)
	require.True(t, page1.HasMore)
	status, page2, body := platformListMerchants(t, surface.BaseURL, opToken, "?limit=2&offset=2")
	require.Equal(t, http.StatusOK, status, string(body))
	require.NotEmpty(t, page2.Data)
	require.Equal(t, 2, page2.Offset)
	seen := map[string]bool{}
	for _, it := range page1.Data {
		seen[it.ID] = true
	}
	for _, it := range page2.Data {
		require.False(t, seen[it.ID], "pages must not overlap (stable ordering)")
	}

	// Bad status filter.
	status, _, _ = platformListMerchants(t, surface.BaseURL, opToken, "?status=bogus")
	require.Equal(t, http.StatusBadRequest, status)
}

func TestPlatformMerchantSoftDeleteRestoreHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	opToken := mintPlatformOperatorToken(t, ctx, surface, controlplane.RootRoleMerchantDirectoryAdmin)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	slug := "plat-del-" + suffix
	m := surface.ProvisionOwnedMerchant(slug)
	t.Cleanup(func() {
		_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.merchants WHERE id = $1", m.MerchantID.UUID())
	})
	merchantURL := surface.BaseURL + "/v1/platform/merchants/" + m.MerchantID.String()
	catalogURL := surface.BaseURL + "/v1/merchant/catalog/products"

	// The merchant's own API key works before the soft delete.
	status, body := requestJSON(t, http.MethodGet, catalogURL, m.APIKey, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	// Soft delete.
	status, body = requestJSON(t, http.MethodDelete, merchantURL, opToken, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var deleted platformMerchantJSON
	require.NoError(t, json.Unmarshal(body, &deleted))
	require.Equal(t, "deleted", deleted.Status)
	require.NotNil(t, deleted.DeletedAt)

	// Default list excludes; ?status=deleted and ?status=all include.
	_, defaultList, _ := platformListMerchants(t, surface.BaseURL, opToken, "?limit=200")
	_, di := findPlatformMerchant(defaultList.Data, slug)
	require.Equal(t, -1, di, "soft-deleted merchant excluded from default list")
	_, deletedList, _ := platformListMerchants(t, surface.BaseURL, opToken, "?limit=200&status=deleted")
	delItem, dj := findPlatformMerchant(deletedList.Data, slug)
	require.NotEqual(t, -1, dj, "soft-deleted merchant visible with status=deleted")
	require.Equal(t, "deleted", delItem.Status)
	_, allList, _ := platformListMerchants(t, surface.BaseURL, opToken, "?limit=200&status=all")
	_, dk := findPlatformMerchant(allList.Data, slug)
	require.NotEqual(t, -1, dk, "soft-deleted merchant visible with status=all")

	// Operators can still inspect the tombstone by id.
	status, body = requestJSON(t, http.MethodGet, merchantURL, opToken, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	// Merchant-scoped auth fails while soft-deleted: credential resolution
	// filters deleted_at IS NULL (controlplane), so the API key 403s.
	status, body = requestJSON(t, http.MethodGet, catalogURL, m.APIKey, nil)
	require.Equal(t, http.StatusForbidden, status, string(body))

	// Idempotent re-delete keeps the original tombstone timestamp.
	status, body = requestJSON(t, http.MethodDelete, merchantURL, opToken, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var redeleted platformMerchantJSON
	require.NoError(t, json.Unmarshal(body, &redeleted))
	require.Equal(t, "deleted", redeleted.Status)
	require.NotNil(t, redeleted.DeletedAt)
	require.True(t, redeleted.DeletedAt.Equal(*deleted.DeletedAt), "re-delete must not move deleted_at")

	// Restore.
	status, body = requestJSON(t, http.MethodPost, merchantURL+"/restore", opToken, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var restored platformMerchantJSON
	require.NoError(t, json.Unmarshal(body, &restored))
	require.Equal(t, "active", restored.Status)
	require.Nil(t, restored.DeletedAt)

	_, afterRestore, _ := platformListMerchants(t, surface.BaseURL, opToken, "?limit=200")
	_, ri := findPlatformMerchant(afterRestore.Data, slug)
	require.NotEqual(t, -1, ri, "restored merchant back in default list")

	// Merchant-scoped auth works again.
	status, body = requestJSON(t, http.MethodGet, catalogURL, m.APIKey, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	// Idempotent restore; unknown ids 404.
	status, _ = requestJSON(t, http.MethodPost, merchantURL+"/restore", opToken, nil)
	require.Equal(t, http.StatusOK, status)
	status, _ = requestJSON(t, http.MethodDelete, surface.BaseURL+"/v1/platform/merchants/"+uuid.NewString(), opToken, nil)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/platform/merchants/"+uuid.NewString()+"/restore", opToken, nil)
	require.Equal(t, http.StatusNotFound, status)
}

func TestPlatformMerchantPermissionsHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	viewerToken := mintPlatformOperatorToken(t, ctx, surface, controlplane.RootRoleMerchantDirectoryViewer)
	noRoleToken := mintPlatformOperatorToken(t, ctx, surface, "")

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	slug := "plat-perm-" + suffix
	m := surface.ProvisionOwnedMerchant(slug)
	t.Cleanup(func() {
		_, _ = h.Pool().Exec(ctx, "DELETE FROM openrails.merchants WHERE id = $1", m.MerchantID.UUID())
	})
	merchantURL := surface.BaseURL + "/v1/platform/merchants/" + m.MerchantID.String()

	// Viewer (root:merchants:read only): reads OK, mutations 403.
	status, _, body := platformListMerchants(t, surface.BaseURL, viewerToken, "")
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestJSON(t, http.MethodGet, merchantURL, viewerToken, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = requestJSON(t, http.MethodDelete, merchantURL, viewerToken, nil)
	require.Equal(t, http.StatusForbidden, status, string(body))
	status, body = requestJSON(t, http.MethodPost, merchantURL+"/restore", viewerToken, nil)
	require.Equal(t, http.StatusForbidden, status, string(body))

	// A user with no root-group role: authenticated but 403 everywhere.
	status, _, _ = platformListMerchants(t, surface.BaseURL, noRoleToken, "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = requestJSON(t, http.MethodDelete, merchantURL, noRoleToken, nil)
	require.Equal(t, http.StatusForbidden, status)

	// A merchant credential (API key) is not a platform principal: 401.
	status, _ = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/platform/merchants", surface.Token, nil)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/platform/merchants", m.APIKey, nil)
	require.Equal(t, http.StatusUnauthorized, status)

	// Unauthenticated: 401.
	status, _ = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/platform/merchants", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)

	// Explicitly-skipped surface (saas #16): no platform create, no patch, no
	// customer/payment/subscription sub-routes.
	for _, probe := range []struct{ method, url string }{
		{http.MethodPost, surface.BaseURL + "/v1/platform/merchants"},
		{http.MethodPatch, merchantURL},
		{http.MethodGet, merchantURL + "/payments"},
		{http.MethodGet, merchantURL + "/customers"},
		{http.MethodGet, merchantURL + "/subscriptions"},
	} {
		status, _ = requestJSON(t, probe.method, probe.url, viewerToken, nil)
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, status,
			fmt.Sprintf("%s %s must not exist", probe.method, probe.url))
	}
}
