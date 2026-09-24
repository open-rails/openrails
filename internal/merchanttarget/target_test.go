package merchanttarget

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type directory struct {
	m         *merchants.Merchant
	err       error
	calls     int
	named     bool
	canonical string
	nameErr   error
	nameCalls int
}

func (d *directory) HasCanonicalNameAuthority() bool { return d.named }
func (d *directory) CanonicalSlug(context.Context, merchant.ID) (string, error) {
	d.nameCalls++
	return d.canonical, d.nameErr
}
func (d *directory) Get(context.Context, merchant.ID) (*merchants.Merchant, error) {
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
	id := merchant.ID(uuid.New())
	active := &merchants.Merchant{ID: id, Slug: "shop", Status: merchants.StatusActive}
	for _, tc := range []struct {
		name    string
		headers http.Header
		dir     *directory
		status  int
	}{
		{"no selector", nil, &directory{m: active}, 400},
		{"both selectors", http.Header{merchant.BindingHeader: {id.String()}, merchant.SlugHeader: {"shop"}}, &directory{m: active}, 400},
		{"duplicate slug header", http.Header{merchant.SlugHeader: {"shop", "shop"}}, &directory{m: active}, 400},
		{"blank id header", http.Header{merchant.BindingHeader: {" "}}, &directory{m: active}, 400},
		{"malformed id", http.Header{merchant.BindingHeader: {"not-a-uuid"}}, &directory{m: active}, 400},
		{"illegal slug", http.Header{merchant.SlugHeader: {"bad_slug!"}}, &directory{m: active}, 400},
		{"not found", http.Header{merchant.SlugHeader: {"shop"}}, &directory{err: merchants.ErrMerchantNotFound}, 404},
		{"inactive", http.Header{merchant.SlugHeader: {"shop"}}, &directory{m: &merchants.Merchant{ID: id, Slug: "shop", Status: merchants.StatusDeleted}}, 404},
		{"directory outage", http.Header{merchant.SlugHeader: {"shop"}}, &directory{err: errors.New("db down")}, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/merchant/settings", nil)
			for k, v := range tc.headers {
				r.Header[http.CanonicalHeaderKey(k)] = v
			}
			_, err := Resolve(r.Context(), r, tc.dir, merchant.ID{}, "")
			requireGate(t, err, tc.status)
		})
	}
	_, err := Resolve(t.Context(), nil, nil, id, "")
	requireGate(t, err, 503)
}

func TestResolvedAliasKeepsCanonicalTargetAndOriginalSelector(t *testing.T) {
	d := &directory{m: &merchants.Merchant{ID: merchant.ID(uuid.New()), Slug: "current", Status: merchants.StatusActive}}
	r := httptest.NewRequest("POST", "/v2/merchant/products", nil)
	r.Header.Set(merchant.SlugHeader, "Former")
	target, err := Resolve(r.Context(), r, d, merchant.ID{}, "")
	require.NoError(t, err)
	require.Equal(t, "current", target.MerchantSlug)
	r = r.WithContext(WithResolved(r.Context(), target))
	require.NoError(t, Assert(r, target))
	captured, err := Resolve(r.Context(), r, d, merchant.ID{}, "")
	require.NoError(t, err)
	require.Equal(t, target, captured)
	require.Equal(t, 1, d.calls, "alias resolves once, never relooked up after authorization")
	r.Header.Set(merchant.SlugHeader, "other")
	requireGate(t, Assert(r, target), 409)
	r.Header.Set(merchant.SlugHeader, "current")
	requireGate(t, Assert(r, target), 409) // even another valid name cannot replace the captured selector
	r.Header.Del(merchant.SlugHeader)
	r.Header.Set(merchant.BindingHeader, uuid.NewString())
	requireGate(t, Assert(r, target), 409)
}

func TestIDSelectionDoesNotRequireOptionalNameAuthority(t *testing.T) {
	id := merchant.ID(uuid.New())
	group := uuid.NewString()
	for _, tc := range []struct {
		name      string
		installed bool
		canonical string
		err       error
		status    int
	}{
		{name: "absent"}, {name: "current", installed: true, canonical: "current"},
		{name: "failed", installed: true, err: errors.New("unavailable"), status: 503},
		{name: "empty", installed: true, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &directory{m: &merchants.Merchant{ID: id, Slug: "stale-reassigned-name", Status: merchants.StatusActive, PermissionGroupID: group}, named: tc.installed, canonical: tc.canonical, nameErr: tc.err}
			r := httptest.NewRequest("GET", "/v1/merchant/settings", nil)
			r.Header.Set(merchant.BindingHeader, id.String())
			target, err := Resolve(r.Context(), r, d, merchant.ID{}, "")
			if tc.status != 0 {
				requireGate(t, err, tc.status)
				return
			}
			require.NoError(t, err)
			require.Equal(t, billingauth.Target{MerchantID: id, MerchantSlug: tc.canonical, AuthorityGroupID: group}, target, "stored mutable name is never presented")
			require.Equal(t, 1, d.calls)
			if !tc.installed {
				require.Zero(t, d.nameCalls, "missing name capability is not an outage")
			}
		})
	}
}

func TestBindingMismatchRefusedBeforeDirectoryLookup(t *testing.T) {
	bound, selected := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	d := &directory{}
	r := httptest.NewRequest("GET", "/v1/merchant/settings", nil)
	r.Header.Set(merchant.BindingHeader, selected.String())
	gate := requireGate(t, func() error { _, err := Resolve(r.Context(), r, d, bound, ""); return err }(), 409)
	require.Contains(t, gate.Message, bound.String())
	require.Contains(t, gate.Message, selected.String())
	require.Zero(t, d.calls)

	// A slug that resolves to a different book than the bound one is also refused.
	d = &directory{m: &merchants.Merchant{ID: selected, Slug: "other", Status: merchants.StatusActive}}
	r = httptest.NewRequest("GET", "/v1/merchant/settings", nil)
	r.Header.Set(merchant.SlugHeader, "other")
	_, err := Resolve(r.Context(), r, d, bound, "")
	requireGate(t, err, 409)
}
