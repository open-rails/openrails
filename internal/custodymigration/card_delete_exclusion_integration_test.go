//go:build integration

package custodymigration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodymigration"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/stretchr/testify/require"
)

func TestCustodyRemapAndAcceptedCardDeletionExcludeEachOther(t *testing.T) {
	for _, ordering := range []string{"native delete first", "HS delete first", "remap first"} {
		t.Run(ordering, func(t *testing.T) {
			f := newCustodyFixture(t)
			_, sub := f.seedPSPVaultedCard(t, "parent-"+uuid.NewString())
			vault := "remap-delete-" + uuid.NewString()
			methodID := f.seedStandaloneCard(t, sub, vault)
			cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureSandbox, HyperSwitch: &config.HyperSwitchConfig{APIBaseURL: "http://127.0.0.1:1"}}
			registry := intents.NewRegistry(intents.NewNMIPaymentMethodDeleteHandler(f.db, nil))
			if ordering == "HS delete first" {
				custodian := uuid.New()
				_, err := f.db.Pool().Exec(f.ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$3,'{"profile_id":"profile_A","public_api_key":"public_A"}','{}')`, custodian, dbtest.TestMerchantID.UUID(), custodian.String())
				require.NoError(t, err)
				_, err = f.db.Pool().Exec(f.ctx, `UPDATE billing.payment_methods SET custodian='hyperswitch',custodian_id=$2,rail_method_ref=$3 WHERE id=$1`, methodID, custodian, "method-"+uuid.NewString())
				require.NoError(t, err)
				_, err = f.db.Pool().Exec(f.ctx, `UPDATE billing.psps SET custodian_id=$2 WHERE id=$1`, f.oldPSP.ID, custodian)
				require.NoError(t, err)
				registry.Register(intents.NewHyperSwitchMethodDeleteHandler(f.db, &paymentmethods.RailPaymentMethodService{DB: f.db, Config: cfg}, clockwork.NewRealClock()))
			}
			original, err := paymentmethods.NewPaymentMethodRepo(f.db).GetByID(f.ctx, methodID)
			require.NoError(t, err)
			runner := &intents.Runner{Store: intents.NewStore(f.db), Registry: registry, Config: cfg}
			through := &intents.PaymentMethodDeleteThrough{Runner: runner}
			export := f.export(custodymigration.ImportedToken{SourceRailCustomerRef: vault, SourceRailMethodRef: original.RailMethodRef, Token: "bt-remapped-" + uuid.NewString()})
			if ordering == "remap first" {
				moved, err := custodymigration.Migrate(f.ctx, f.opts(export, true))
				require.NoError(t, err)
				require.Equal(t, custodymigration.OutcomeRemapped, moved.Rows[0].Outcome)
				_, err = through.ExecutePaymentMethodDelete(f.ctx, original)
				require.ErrorIs(t, err, paymentmethods.ErrPaymentMethodDeleteUnsafe)
				_, err = intents.NewStore(f.db).GetByIdempotencyKey(f.ctx, intents.NMIPaymentMethodDeleteIdempotencyKey(methodID))
				require.ErrorIs(t, err, pgx.ErrNoRows)
				current := f.method(t, methodID)
				require.Equal(t, models.CustodianBasisTheory, current.Custodian)
				require.Empty(t, current.ParkReason)
			} else {
				pending, err := through.ExecutePaymentMethodDelete(f.ctx, original)
				require.NoError(t, err)
				require.False(t, pending.Done)
				for _, apply := range []bool{false, true} {
					moved, err := custodymigration.Migrate(f.ctx, f.opts(export, apply))
					require.NoError(t, err)
					require.Equal(t, custodymigration.OutcomeBlocked, moved.Rows[0].Outcome)
					require.Equal(t, custodymigration.ReasonOperationUnresolved, moved.Rows[0].Reason)
				}
				current := f.method(t, methodID)
				require.Equal(t, original.PspID, current.PspID)
				require.Equal(t, original.RailMethodRef, current.RailMethodRef)
				require.Contains(t, current.ParkReason, "delete:")
			}
		})
	}
}
