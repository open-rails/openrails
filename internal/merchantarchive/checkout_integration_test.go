//go:build integration

package merchantarchive

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCompletedCheckoutServiceRoundTrip(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "archive_checkout")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	seedBook(t, source, id)
	ctx, release, err := source.WithMerchantConn(merchant.WithID(t.Context(), id))
	require.NoError(t, err)
	var customer, price, psp, payment, subscription uuid.UUID
	require.NoError(t, source.Qx(ctx).QueryRow(ctx, `SELECT customer_id,price_id,psp_id,id,subscription_id FROM openrails.payments WHERE merchant_id=$1 AND transaction_id='charge-1'`, id.UUID()).Scan(&customer, &price, &psp, &payment, &subscription))
	now := time.Now().UTC().Truncate(time.Microsecond)
	expiry := now.Add(time.Hour)
	session := &models.CheckoutSession{ID: uuid.New(), CustomerID: customer, PriceID: price, PspID: psp, Mode: models.CheckoutSessionModeSubscription, Rail: models.RailNMI, Status: models.CheckoutSessionStatusCreated, Amount: 1000000, Currency: "USD", ExpiresAt: &expiry, CreatedAt: now, UpdatedAt: now, RailFields: map[string]any{"rail": "nmi", "psp": "primary", "payment_method_id": "pm_retained"}, RailState: map[string]any{"_openrails_request_fingerprint": "checkout-service-request"}, RoutingReason: &models.CheckoutRoutingReason{Policy: "default", Selected: "primary", Rail: "nmi"}}
	repo := checkout.NewCheckoutSessionRepo(source)
	require.NoError(t, repo.Create(ctx, session))
	service := checkout.NewCheckoutSessionService(source, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	require.NoError(t, service.MarkSucceededWithSubscription(ctx, session.ID, payment, "charge-1", subscription))
	before, err := repo.GetByID(ctx, session.ID)
	require.NoError(t, err)
	require.Equal(t, models.CheckoutSessionStatusSucceeded, before.Status)
	release()
	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &artifact))
	_, err = Restore(t.Context(), target, id, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	targetCtx, releaseTarget, err := target.WithMerchantConn(merchant.WithID(t.Context(), id))
	require.NoError(t, err)
	defer releaseTarget()
	after, err := checkout.NewCheckoutSessionRepo(target).GetByID(targetCtx, session.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	// The same terminal transition stays a no-op after transfer; it cannot create
	// a new payment or reset the request's durable completion/fingerprint.
	restoredService := checkout.NewCheckoutSessionService(target, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	require.NoError(t, restoredService.MarkSucceededWithSubscription(targetCtx, session.ID, payment, "charge-1", subscription))
	again, err := checkout.NewCheckoutSessionRepo(target).GetByID(targetCtx, session.ID)
	require.NoError(t, err)
	require.Equal(t, after, again)
}
