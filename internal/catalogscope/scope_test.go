package catalogscope

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestOwnerScopeRequiresMerchantAuthority(t *testing.T) {
	mid := merchant.ID(uuid.New())
	authorized := merchant.WithID(t.Context(), mid)
	scope := Scope{MerchantID: mid, CatalogID: uuid.New(), OwnerSubject: "作者 / External:123"}

	for name, tc := range map[string]struct {
		ctx   context.Context
		scope Scope
	}{
		"no merchant":      {t.Context(), scope},
		"other merchant":   {merchant.WithID(t.Context(), merchant.ID(uuid.New())), scope},
		"zero catalog":     {authorized, Scope{MerchantID: mid, OwnerSubject: "a"}},
		"zero merchant":    {authorized, Scope{CatalogID: uuid.New(), OwnerSubject: "a"}},
		"empty subject":    {authorized, Scope{MerchantID: mid, CatalogID: uuid.New()}},
		"NUL subject":      {authorized, Scope{MerchantID: mid, CatalogID: uuid.New(), OwnerSubject: "a\x00b"}},
		"invalid utf8 sub": {authorized, Scope{MerchantID: mid, CatalogID: uuid.New(), OwnerSubject: "\xff"}},
	} {
		_, err := WithOwner(tc.ctx, tc.scope)
		require.Error(t, err, name)
	}

	ctx, err := WithOwner(authorized, scope)
	require.NoError(t, err)
	got, ok := FromContext(ctx)
	require.True(t, ok)
	require.Equal(t, scope, got, "subject is preserved exactly, not normalized")
	require.Equal(t, &scope.CatalogID, QueryID(ctx))
	require.Nil(t, QueryID(t.Context()))
	// A malformed internal scope must never become an unfiltered query.
	require.Equal(t, &uuid.Nil, QueryID(context.WithValue(t.Context(), ownerKey{}, Scope{})))
}
