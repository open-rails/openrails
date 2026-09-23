package merchanttarget

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type forwardingDirectory struct {
	m     *merchants.Merchant
	calls int
}

func (d *forwardingDirectory) CanonicalSlug(context.Context, merchant.ID) (string, error) {
	return d.m.Slug, nil
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
