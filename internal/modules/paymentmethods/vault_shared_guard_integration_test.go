//go:build integration

package paymentmethods

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

type noSubsReader struct{}

func (noSubsReader) GetPaginatedByUserID(context.Context, string, int, int) ([]models.Subscription, int, error) {
	return nil, 0, nil
}

// TestDeleteVaultSharedVaultScopesToBillingEntry (#682): a whole-vault delete
// would destroy every billing entry in the vault, so when another
// payment-method row shares the vault id (an imported multi-card vault),
// DeleteVault must delete ONLY this row's billing entry
// (DELETE /v5/customers/{vault}/billing/{billing_id}) and never the vault —
// the sibling card survives. A shared row WITHOUT a billing id (cannot
// identify which entry) still refuses outright.
func TestDeleteVaultSharedVaultScopesToBillingEntry(t *testing.T) {
	pool := dbtest.SharedPGXPool(t)
	database, err := db.NewWithPGXPool(pool, "openrails")
	require.NoError(t, err)
	ctx := dbtest.WithTestMerchant(context.Background())

	userID := uuid.NewString()
	customerID, err := db.EnsureCustomerID(ctx, database.Qx(ctx), uuid.Nil, userID)
	require.NoError(t, err)

	sharedVault := "vault-shared-" + uuid.NewString()[:8]
	pmRepo := NewPaymentMethodRepo(database)
	mk := func(methodRef string) *models.PaymentMethod {
		pm := &models.PaymentMethod{
			ID:                   uuid.New(),
			CustomerID:           customerID,
			Rail:                 models.RailNMI,
			RailCustomerRef:      sharedVault,
			RailMethodRef:        methodRef,
			RebillDriver:         models.RebillDriverProvider,
			InitialTransactionID: "txn-" + uuid.NewString()[:8],
			CreatedAt:            time.Now().UTC(),
			UpdatedAt:            time.Now().UTC(),
		}
		require.NoError(t, pmRepo.Create(ctx, pm))
		t.Cleanup(func() { _ = pmRepo.Delete(ctx, pm.ID) })
		return pm
	}
	pmA := mk("bill-a-" + uuid.NewString()[:8])
	pmB := mk("bill-b-" + uuid.NewString()[:8])

	var entryDeletes, vaultDeletes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/customers/") && r.URL.Path[:11] == "/customers/" && containsBilling(r.URL.Path):
			entryDeletes = append(entryDeletes, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			vaultDeletes = append(vaultDeletes, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)

	svc := &VaultService{
		SubscriptionService:  noSubsReader{},
		PaymentMethodService: NewPaymentMethodService(database),
		DB:                   database,
		Config:               vaultTestConfig(true),
		Rails:                vaultTestRails("test-key"),
		newNMIClient: func(provider string, cfg *config.NMIProviderSettings, testMode bool) (*nmi.NMIClient, error) {
			client, err := nmi.NewClient(provider, cfg, testMode)
			if err != nil {
				return nil, err
			}
			client.V5BaseURL = server.URL
			client.DirectPostURL = server.URL
			return client, nil
		},
	}

	require.NoError(t, svc.DeleteVault(ctx, pmA), "shared vault deletes must scope to the billing entry")
	require.Len(t, entryDeletes, 1, "exactly one billing-entry delete")
	require.Contains(t, entryDeletes[0], "/customers/"+sharedVault+"/billing/"+pmA.RailMethodRef)
	require.Empty(t, vaultDeletes, "the shared vault itself must NEVER be deleted")
	if _, err := NewPaymentMethodRepo(database).GetByID(ctx, pmA.ID); err == nil {
		t.Fatal("deleted row must be gone locally")
	}
	if _, err := NewPaymentMethodRepo(database).GetByID(ctx, pmB.ID); err != nil {
		t.Fatalf("sibling row must survive: %v", err)
	}

	// A shared row with NO billing id cannot be scoped — refuse outright.
	pmC := mk("")
	pmD := mk("bill-d-" + uuid.NewString()[:8])
	_ = pmD
	err = svc.DeleteVault(ctx, pmC)
	require.Error(t, err, "shared vault without a billing id must refuse")
	require.Contains(t, err.Error(), "no billing id", "refusal names the missing handle: %v", err)
}

func containsBilling(path string) bool {
	for i := 0; i+9 <= len(path); i++ {
		if path[i:i+9] == "/billing/" {
			return true
		}
	}
	return false
}
