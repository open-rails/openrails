package checkout

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakePSPCatalog is a static Layer-B stand-in implementing the resolver
// capabilities resolveRailTarget consumes: key-first lookup + the #848
// unambiguous rail-kind list.
type fakePSPCatalog struct {
	scopes  []merchants.PSPScope
	keyErr  error
	listErr error
}

func (f fakePSPCatalog) ActivePSPSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (f fakePSPCatalog) PSPScopeByKey(_ context.Context, _ merchant.ID, key, _ string) (merchants.PSPScope, bool, error) {
	if f.keyErr != nil {
		return merchants.PSPScope{}, false, f.keyErr
	}
	for _, s := range f.scopes {
		if strings.EqualFold(s.Key, key) {
			return s, true, nil
		}
	}
	return merchants.PSPScope{}, false, nil
}

func (f fakePSPCatalog) ActivePSPScopesForRail(_ context.Context, _ merchant.ID, rail, _ string) ([]merchants.PSPScope, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []merchants.PSPScope
	for _, s := range f.scopes {
		if strings.EqualFold(s.Rail, rail) {
			out = append(out, s)
		}
	}
	return out, nil
}

type keyOnlyPSPCatalog struct{}

func (keyOnlyPSPCatalog) ActivePSPSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (keyOnlyPSPCatalog) PSPScopeByKey(context.Context, merchant.ID, string, string) (merchants.PSPScope, bool, error) {
	return merchants.PSPScope{}, false, nil
}

func railTargetTestService(scopes ...merchants.PSPScope) *CheckoutService {
	return &CheckoutService{
		Config:          &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
		ProviderSecrets: fakePSPCatalog{scopes: scopes},
	}
}

func TestResolveRailTarget_PSPKeyWireValue(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	mobius := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", Environment: "live", AccountID: "gw-1", Key: "mobius-sandbox"}
	svc := railTargetTestService(mobius)

	target, err := svc.resolveRailTarget(ctx, "mobius-sandbox")
	require.NoError(t, err)
	require.Equal(t, "mobius-sandbox", target.PSP)
	require.Equal(t, "nmi", target.Rail)
	require.NotNil(t, target.Scope)
	require.Equal(t, mobius.ID, target.Scope.ID)
}

func TestResolveRailTarget_RailKindSingleArmedFallback(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	mobius := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", Environment: "live", AccountID: "gw-1", Key: "mobius-sandbox"}
	svc := railTargetTestService(mobius)

	target, err := svc.resolveRailTarget(ctx, "nmi")
	require.NoError(t, err)
	require.Equal(t, "mobius-sandbox", target.PSP, "the single armed PSP's key is adopted")
	require.Equal(t, "nmi", target.Rail)
	require.NotNil(t, target.Scope)
	require.Equal(t, mobius.ID, target.Scope.ID)
}

func TestResolveRailTarget_RailKindAmbiguousNamesKeys(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := railTargetTestService(
		merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius-sandbox"},
		merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-2", Key: "paykings"},
	)

	_, err := svc.resolveRailTarget(ctx, "nmi")
	var ambiguous *AmbiguousRailError
	require.ErrorAs(t, err, &ambiguous)
	require.Equal(t, "nmi", ambiguous.Rail)
	require.ElementsMatch(t, []string{"mobius-sandbox", "paykings"}, ambiguous.Keys)
	require.Contains(t, err.Error(), "mobius-sandbox")
	require.Contains(t, err.Error(), "paykings")

	// A PSP key still resolves exactly even when the kind is ambiguous.
	target, err := svc.resolveRailTarget(ctx, "paykings")
	require.NoError(t, err)
	require.Equal(t, "paykings", target.PSP)
	require.Equal(t, "gw-2", target.Scope.AccountID)
}

func TestResolveRailTarget_UnknownSelector(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := railTargetTestService()

	_, err := svc.resolveRailTarget(ctx, "bogus")
	var unknown *UnknownRailError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, "bogus", unknown.Selector)
}

func TestResolveRailTarget_FailsClosedWithoutExactPSPIdentity(t *testing.T) {
	t.Parallel()
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	lookupErr := errors.New("catalog unavailable")

	tests := []struct {
		name string
		svc  *CheckoutService
		ctx  context.Context
		want string
	}{
		{name: "resolver not wired", svc: &CheckoutService{}, ctx: ctx, want: "resolution is not configured"},
		{name: "rail list capability missing", svc: &CheckoutService{ProviderSecrets: keyOnlyPSPCatalog{}}, ctx: ctx, want: "rail resolution is not configured"},
		{name: "key lookup fails", svc: &CheckoutService{ProviderSecrets: fakePSPCatalog{keyErr: lookupErr}}, ctx: ctx, want: "catalog unavailable"},
		{name: "rail lookup fails", svc: &CheckoutService{ProviderSecrets: fakePSPCatalog{listErr: lookupErr}}, ctx: ctx, want: "catalog unavailable"},
		{name: "rail has no armed provider", svc: railTargetTestService(), ctx: ctx, want: "has no armed PSP"},
		{name: "merchant context missing", svc: railTargetTestService(), ctx: context.Background(), want: "no merchant resolved on context"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, err := tt.svc.resolveRailTarget(tt.ctx, "nmi")
			require.ErrorContains(t, err, tt.want)
			require.Nil(t, target.Scope)
		})
	}
}

func TestResolveRailTargetForPSP_SelectsPersistedAccount(t *testing.T) {
	t.Parallel()
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	mobius := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius"}
	paykings := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-2", Key: "paykings"}
	svc := railTargetTestService(mobius, paykings)

	target, err := svc.resolveRailTargetForPSP(ctx, "nmi", paykings.ID)
	require.NoError(t, err)
	require.Equal(t, "paykings", target.PSP)
	require.Equal(t, paykings.ID, target.Scope.ID)

	_, err = svc.resolveRailTargetForPSP(ctx, "nmi", uuid.New())
	require.ErrorContains(t, err, "is not armed")
}

func TestCheckoutRailUsable(t *testing.T) {
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	svc := railTargetTestService(
		merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius-sandbox"},
		merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-2", Key: "paykings"},
		merchants.PSPScope{ID: uuid.New(), Rail: "ccbill", AccountID: "945280-0000", Key: "ccbill"},
	)

	require.NoError(t, svc.CheckoutRailUsable(ctx, "mobius-sandbox"), "PSP key")
	require.NoError(t, svc.CheckoutRailUsable(ctx, "ccbill"), "rail kind with one armed PSP")

	err := svc.CheckoutRailUsable(ctx, "nmi")
	require.Error(t, err, "rail kind with two armed PSPs")
	require.Contains(t, err.Error(), "mobius-sandbox")
	require.Contains(t, err.Error(), "paykings")

	require.ErrorContains(t, svc.CheckoutRailUsable(ctx, "solana"), "has no armed PSP", "known rail with nothing armed")
	require.ErrorContains(t, svc.CheckoutRailUsable(ctx, "bogus"), "unknown payment provider")
}
