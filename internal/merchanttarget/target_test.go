package merchanttarget

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type forwardingDirectory struct {
	m         *merchants.Merchant
	calls     int
	named     bool
	canonical string
	nameErr   error
	nameCalls int
}

func (d *forwardingDirectory) HasCanonicalNameAuthority() bool { return d.named }
func (d *forwardingDirectory) CanonicalSlug(context.Context, merchant.ID) (string, error) {
	d.nameCalls++
	return d.canonical, d.nameErr
}
func (d *forwardingDirectory) Get(context.Context, merchant.ID) (*merchants.Merchant, error) {
	d.calls++
	return d.m, nil
}
func (d *forwardingDirectory) GetBySlug(context.Context, string) (*merchants.Merchant, error) {
	d.calls++
	return d.m, nil
}
func TestResolvedAliasKeepsCanonicalTargetAndOriginalSelector(t *testing.T) {
	d := &forwardingDirectory{m: &merchants.Merchant{ID: merchant.ID(uuid.New()), Slug: "current", Status: merchants.StatusActive}}
	r := httptest.NewRequest("POST", "/v2/merchant/products", nil)
	r.Header.Set(merchant.SlugHeader, "former")
	target, err := Resolve(r.Context(), r, d, merchant.ID{}, "")
	require.NoError(t, err)
	require.Equal(t, "current", target.MerchantSlug)
	r = r.WithContext(WithResolved(r.Context(), target))
	require.NoError(t, Assert(r, target))
	captured, err := Resolve(r.Context(), r, d, merchant.ID{}, "")
	require.NoError(t, err)
	require.Equal(t, target, captured)
	require.Equal(t, 1, d.calls, "alias is resolved once, never relooked up after authorization")
	r.Header.Set(merchant.SlugHeader, "other")
	require.Error(t, Assert(r, target))
	r.Header.Set(merchant.SlugHeader, "current")
	require.Error(t, Assert(r, target), "even another valid name cannot replace the captured request selector")
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
			d := &forwardingDirectory{m: &merchants.Merchant{ID: id, Slug: "stale-reassigned-name", Status: merchants.StatusActive, PermissionGroupID: group}, named: tc.installed, canonical: tc.canonical, nameErr: tc.err}
			r := httptest.NewRequest("GET", "/v1/merchant/settings", nil)
			r.Header.Set(merchant.BindingHeader, id.String())
			target, err := Resolve(r.Context(), r, d, merchant.ID{}, "")
			if tc.status != 0 {
				var failure billingauth.GateError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, tc.status, failure.Status)
				return
			}
			require.NoError(t, err)
			require.Equal(t, id, target.MerchantID)
			require.Equal(t, group, target.AuthorityGroupID)
			require.Equal(t, tc.canonical, target.MerchantSlug)
			require.Equal(t, 1, d.calls, "only immutable ID is read, never a stored mutable name")
			if !tc.installed {
				require.Zero(t, d.nameCalls, "missing name capability is not an unavailable configured service")
			}
		})
	}
}

func TestIDMismatchRefusedBeforeDirectoryLookup(t *testing.T) {
	bound, selected := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	d := &forwardingDirectory{}
	r := httptest.NewRequest("GET", "/v1/merchant/settings", nil)
	r.Header.Set(merchant.BindingHeader, selected.String())
	_, err := Resolve(r.Context(), r, d, bound, "")
	var failure billingauth.GateError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, 409, failure.Status)
	require.Contains(t, failure.Message, bound.String())
	require.Contains(t, failure.Message, selected.String())
	require.Zero(t, d.calls)
}
