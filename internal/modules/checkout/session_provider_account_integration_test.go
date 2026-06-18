//go:build integration

package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type checkoutProviderAccountResolver struct{}

func (checkoutProviderAccountResolver) ResolveProviderAccount(context.Context, string) (intents.ProviderAccountIdentity, bool) {
	return intents.ProviderAccountIdentity{
		ProviderKey:  "stripe_primary",
		ProviderType: config.ProcessorTypeStripe,
		AccountID:    "acct_checkout_primary",
		Evidence:     map[string]any{"source": "test"},
	}, true
}

type checkoutProviderAccountExecutor struct{}

func (checkoutProviderAccountExecutor) Checkout(context.Context, *CheckoutRequest, *UserIdentity) (*CheckoutResponse, error) {
	return &CheckoutResponse{
		Status:      "redirect_required",
		RedirectURL: "https://checkout.stripe.test/session",
	}, nil
}

func (checkoutProviderAccountExecutor) RegisterPurchase(context.Context, *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error) {
	return nil, nil
}

func (checkoutProviderAccountExecutor) CheckSubscriptionConflict(context.Context, string, *models.Price, *models.Product) (*SubscriptionConflict, error) {
	return nil, nil
}

func TestCheckoutSessionStampsPrimaryProviderAccount(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	dbtest.EnsureTestMerchant(context.Background(), t, dbi.Pool())

	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		now := time.Now().UTC().Truncate(time.Second)
		productID := uuid.New()
		priceID := uuid.New()
		product := &models.Product{
			ID:          productID,
			Slug:        "checkout_provider_account_" + uuid.NewString(),
			DisplayName: "Provider Account Checkout",
			Status:      models.CatalogStatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		price := &models.Price{
			ID:        priceID,
			ProductID: productID,
			Status:    models.CatalogStatusActive,
			Amount:    1000,
			Currency:  "USD",
			CreatedAt: now,
			UpdatedAt: now,
		}
		insertProductAndPrice(ctx, t, dbi.Qx(ctx), product, price)

		cfg := &config.Config{Processors: map[string]*config.ProcessorConfig{
			"stripe_primary": {Type: config.ProcessorTypeStripe, Role: config.ProcessorRolePrimary},
		}}
		svc := NewCheckoutSessionService(
			dbi,
			catalog.NewPriceService(dbi),
			catalog.NewProductService(dbi),
			nil,
			nil,
			checkoutProviderAccountExecutor{},
			nil,
			nil,
			nil,
			nil,
			cfg,
		)
		svc.SetProviderAccounts(checkoutProviderAccountResolver{})

		resp, err := svc.CreateSession(ctx, &CheckoutSessionCreateRequest{
			PriceID: api.FormatPriceID(priceID),
			Payment: CheckoutSessionPaymentRequest{
				Processor: config.ProcessorTypeStripe,
			},
		}, &UserIdentity{ID: uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, "requires_action", resp.Status)

		sessionID, err := api.ParseCheckoutSessionID(resp.ID)
		require.NoError(t, err)
		session, err := svc.repo.GetByID(ctx, sessionID)
		require.NoError(t, err)
		require.NotNil(t, session.ProviderAccountID)

		account, err := dbi.Gen(ctx).GetProviderAccount(ctx, *session.ProviderAccountID)
		require.NoError(t, err)
		require.Equal(t, "stripe", account.ProviderType)
		require.Equal(t, "acct_checkout_primary", account.AccountID)
		require.Equal(t, "primary", account.Role)
		return nil
	}))
}
