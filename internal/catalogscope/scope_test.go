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
	scope := Scope{MerchantID: mid, CatalogID: uuid.New(), OwnerSubject: "作者 / external:123"}
	_, err := WithOwner(t.Context(), scope)
	require.Error(t, err)
	_, err = WithOwner(merchant.WithID(t.Context(), merchant.ID(uuid.New())), scope)
	require.Error(t, err)
	ctx, err := WithOwner(merchant.WithID(t.Context(), mid), scope)
	require.NoError(t, err)
	got, ok := FromContext(ctx)
	require.True(t, ok)
	require.Equal(t, scope, got)
	require.Equal(t, &scope.CatalogID, QueryID(ctx))
	require.Nil(t, QueryID(t.Context()))
	// Even a malformed internal scope must never turn into an unfiltered query.
	invalid := context.WithValue(t.Context(), ownerKey{}, Scope{})
	require.Equal(t, &uuid.Nil, QueryID(invalid))
}
