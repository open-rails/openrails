package embed

import (
	"context"
	"reflect"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestMerchantDeclarationRejectedBeforeOpeningResources(t *testing.T) {
	for name, declaration := range map[string]*MerchantDeclaration{
		"missing_slug":    {},
		"missing_key":     {Slug: "merchant", PSPs: []PSPDeclaration{{Rail: "stripe", AccountID: "acct_example"}}},
		"missing_rail":    {Slug: "merchant", PSPs: []PSPDeclaration{{Key: "primary", AccountID: "acct_example"}}},
		"missing_account": {Slug: "merchant", PSPs: []PSPDeclaration{{Key: "primary", Rail: "stripe"}}},
	} {
		t.Run(name, func(t *testing.T) {
			runtime, err := New(context.Background(), Options{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, Merchant: declaration})
			require.Nil(t, runtime)
			require.ErrorContains(t, err, "Merchant.")
		})
	}
}

func TestRuntimeDoesNotExposeMerchantMutations(t *testing.T) {
	typ := reflect.TypeOf((*Runtime)(nil))
	for _, name := range []string{"UpsertMerchantConfig", "DeclarePSP"} {
		_, present := typ.MethodByName(name)
		require.False(t, present, "merchant declarations belong in constructor options")
	}
}
