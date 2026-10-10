package merchanttarget

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

type directory struct {
	m     *merchants.Merchant
	err   error
	calls int
}

func (d *directory) Get(context.Context, billing.MerchantID) (*merchants.Merchant, error) {
	d.calls++
	return d.m, d.err
}
func (d *directory) GetBySlug(context.Context, string) (*merchants.Merchant, error) {
	d.calls++
	return d.m, d.err
}

func requireGate(t *testing.T, err error, status int) billingauth.GateError {
	t.Helper()
	var gate billingauth.GateError
	require.ErrorAs(t, err, &gate)
	require.Equal(t, status, gate.Status, gate.Message)
	return gate
}

func TestSelectorFailures(t *testing.T) {
	id := billing.MerchantID(uuid.New())
	active := &merchants.Merchant{ID: id, Slug: "shop", Status: merchants.StatusActive}
	for _, tc := range []struct {
		name    string
		headers http.Header
		dir     *directory
		status  int
	}{
		{"no selector", nil, &directory{m: active}, 400},
		{"two selectors", http.Header{merchant.SelectorHeader: {"id:" + id.String(), "shop"}}, &directory{m: active}, 400},
		{"duplicate selector", http.Header{merchant.SelectorHeader: {"shop", "shop"}}, &directory{m: active}, 400},
		{"blank selector", http.Header{merchant.SelectorHeader: {" "}}, &directory{m: active}, 400},
		{"blank id", http.Header{merchant.SelectorHeader: {"id:"}}, &directory{m: active}, 400},
		{"malformed id", http.Header{merchant.SelectorHeader: {"id:not-a-uuid"}}, &directory{m: active}, 400},
		{"illegal slug", http.Header{merchant.SelectorHeader: {"bad_slug!"}}, &directory{m: active}, 400},
		{"not found", http.Header{merchant.SelectorHeader: {"shop"}}, &directory{err: merchants.ErrMerchantNotFound}, 404},
		{"inactive", http.Header{merchant.SelectorHeader: {"shop"}}, &directory{m: &merchants.Merchant{ID: id, Slug: "shop", Status: merchants.StatusDeleted}}, 404},
		{"directory outage", http.Header{merchant.SelectorHeader: {"shop"}}, &directory{err: errors.New("db down")}, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/admin/configuration", nil)
			for k, v := range tc.headers {
				r.Header[http.CanonicalHeaderKey(k)] = v
			}
			_, err := Resolve(r.Context(), r, tc.dir, billing.MerchantID{}, "")
			requireGate(t, err, tc.status)
		})
	}
	_, err := Resolve(t.Context(), nil, nil, id, "")
	requireGate(t, err, 503)
}

func TestResolvedAliasKeepsCanonicalTargetAndOriginalSelector(t *testing.T) {
	d := &directory{m: &merchants.Merchant{ID: billing.MerchantID(uuid.New()), Slug: "current", Status: merchants.StatusActive}}
	r := httptest.NewRequest("POST", "/v1/admin/products", nil)
	r.Header.Set(merchant.SelectorHeader, "Former")
	target, err := Resolve(r.Context(), r, d, billing.MerchantID{}, "")
	require.NoError(t, err)
	require.Equal(t, "current", target.MerchantSlug)
	r = r.WithContext(WithResolved(r.Context(), target))
	require.NoError(t, Assert(r, target))
	captured, err := Resolve(r.Context(), r, d, billing.MerchantID{}, "")
	require.NoError(t, err)
	require.Equal(t, target, captured)
	require.Equal(t, 1, d.calls, "alias resolves once, never relooked up after authorization")
	r.Header.Set(merchant.SelectorHeader, "other")
	requireGate(t, Assert(r, target), 409)
	r.Header.Set(merchant.SelectorHeader, "current")
	requireGate(t, Assert(r, target), 409) // even another valid name cannot replace the captured selector
	r.Header.Set(merchant.SelectorHeader, "id:"+uuid.NewString())
	requireGate(t, Assert(r, target), 409)
}

// The stored name is the merchant's current name: ID selection presents it
// without another lookup.
func TestIDSelectionPresentsTheStoredName(t *testing.T) {
	id := billing.MerchantID(uuid.New())
	group := uuid.NewString()
	d := &directory{m: &merchants.Merchant{ID: id, Slug: "current", Status: merchants.StatusActive, PermissionGroupID: group}}
	r := httptest.NewRequest("GET", "/v1/admin/configuration", nil)
	r.Header.Set(merchant.SelectorHeader, "id:"+id.String())
	target, err := Resolve(r.Context(), r, d, billing.MerchantID{}, "")
	require.NoError(t, err)
	require.Equal(t, billingauth.Target{MerchantID: id, MerchantSlug: "current", AuthorityGroupID: group}, target)
	require.Equal(t, 1, d.calls)
}

func TestBindingMismatchRefusedBeforeDirectoryLookup(t *testing.T) {
	bound, selected := billing.MerchantID(uuid.New()), billing.MerchantID(uuid.New())
	d := &directory{}
	r := httptest.NewRequest("GET", "/v1/admin/configuration", nil)
	r.Header.Set(merchant.SelectorHeader, "id:"+selected.String())
	gate := requireGate(t, func() error { _, err := Resolve(r.Context(), r, d, bound, ""); return err }(), 409)
	require.Contains(t, gate.Message, bound.String())
	require.Contains(t, gate.Message, selected.String())
	require.Zero(t, d.calls)

	// A slug that resolves to a different book than the bound one is also refused.
	d = &directory{m: &merchants.Merchant{ID: selected, Slug: "other", Status: merchants.StatusActive}}
	r = httptest.NewRequest("GET", "/v1/admin/configuration", nil)
	r.Header.Set(merchant.SelectorHeader, "other")
	_, err := Resolve(r.Context(), r, d, bound, "")
	requireGate(t, err, 409)
}
