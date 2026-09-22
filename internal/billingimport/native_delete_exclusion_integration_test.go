//go:build integration

package billingimport_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestDeclaredImportCannotAttachBehindAcceptedNativeVaultDeletion(t *testing.T) {
	d := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)
	psp := dbtest.EnsureTestPSP(ctx, t, d.Pool(), dbtest.TestMerchantID.UUID(), "nmi")
	customer := dbtest.EnsureCustomerIDPgx(ctx, t, d.Pool(), uuid.NewString())
	pm := &models.PaymentMethod{ID: uuid.New(), CustomerID: customer, PspID: psp, Rail: models.RailNMI, Custodian: models.CustodianPSP, RailCustomerRef: "import-delete-" + uuid.NewString(), RailMethodRef: "accepted-entry"}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(d).Create(ctx, pm))
	runner := &intents.Runner{Store: intents.NewStore(d), Registry: intents.NewRegistry(intents.NewNMIPaymentMethodDeleteHandler(d, nil)), Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}}
	out, err := (&intents.PaymentMethodDeleteThrough{Runner: runner}).ExecutePaymentMethodDelete(ctx, pm)
	require.NoError(t, err)
	require.False(t, out.Done)
	_, err = billingimport.Import(ctx, billingimport.Options{DB: d, MerchantID: dbtest.TestMerchantID, Book: billingimport.DeclaredBilling{AsOf: time.Now().UTC(), DefaultPSP: billingimport.PSPRef{Key: "nmi"}, Customers: []billingimport.DeclaredCustomer{{Customer: openrails.CustomerID(customer)}}, PaymentMethods: []billingimport.DeclaredPaymentMethod{{Customer: openrails.CustomerID(customer), Rail: "nmi", RailCustomerRef: pm.RailCustomerRef, RailMethodRef: "late-entry"}}}})
	require.ErrorIs(t, err, paymentmethods.ErrPaymentMethodDeleteProcessing)
	var aliases int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.payment_methods WHERE merchant_id=$1 AND psp_id=$2 AND rail_customer_ref=$3`, dbtest.TestMerchantID.UUID(), psp, pm.RailCustomerRef).Scan(&aliases))
	require.Equal(t, 1, aliases)
}
