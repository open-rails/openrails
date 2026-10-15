package intents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
)

func TestManualCardDeleteCannotRunThroughGenericExecutor(t *testing.T) {
	for _, kind := range []string{TypeNMIPaymentMethodDelete, TypePlatformMethodDelete, TypeHyperSwitchMethodDelete} {
		t.Run(kind, func(t *testing.T) {
			in := testIntent(kind, OriginUser, 1)
			in.Status = StatusPending
			store := &fakeLedger{row: in, due: []gen.BillingProviderIntent{in}}
			h := &fakeHandler{typ: kind, execute: Succeeded(nil), verify: Succeeded(nil)}
			runner := &Runner{Store: store, Registry: NewRegistry(h), Config: modeFull}
			_, err := runner.ExecuteByID(context.Background(), in.ID)
			require.ErrorIs(t, err, ErrManualPaymentMethodDeleteRequired)
			_, err = runner.RunExecuteOnce(context.Background())
			require.NoError(t, err)
			require.Zero(t, h.executed)
			store.row.Status = StatusUnknownNeedsVerify
			store.dueVerify = []gen.BillingProviderIntent{store.row}
			_, err = runner.VerifyByID(context.Background(), in.ID)
			require.ErrorIs(t, err, ErrManualPaymentMethodDeleteRequired)
			_, err = runner.RunVerifyOnce(context.Background())
			require.NoError(t, err)
			require.Zero(t, h.verified)
		})
	}
}

func TestManualCustodianDeleteKeys(t *testing.T) {
	for _, kind := range []string{TypePlatformMethodDelete, TypeHyperSwitchMethodDelete} {
		t.Run(kind, func(t *testing.T) {
			method, customer, custodian := uuid.New(), uuid.New(), uuid.New()
			instrument := charge.FrozenInstrument{CustodianID: &custodian, RailMethodRef: "owned-method"}
			in := gen.BillingProviderIntent{ID: uuid.New(), MerchantID: uuid.New(), IntentType: kind, CustodianID: &custodian, Origin: string(OriginUser)}
			var payload any
			var decode func(gen.BillingProviderIntent) error
			if kind == TypePlatformMethodDelete {
				in.Rail, instrument.Custodian = models.CustodianPlatform, models.CustodianPlatform
				payload = PlatformMethodDeletePayload{CustomerID: customer, PaymentMethodID: method, Instrument: instrument, TenantID: "owned-tenant", Environment: "test"}
				decode = func(in gen.BillingProviderIntent) error { _, err := DecodePlatformMethodDelete(in); return err }
			} else {
				in.Rail, instrument.Custodian = models.CustodianHyperSwitch, models.CustodianHyperSwitch
				instrument.RailCustomerRef = "owned-customer"
				payload = HyperSwitchMethodDeletePayload{CustomerID: customer, PaymentMethodID: method, Instrument: instrument, Environment: "test", Binding: charge.HyperSwitchBinding{AccountID: "owned-account", ProfileID: "owned-profile", APIBaseURL: "https://example.test"}}
				decode = func(in gen.BillingProviderIntent) error { _, err := DecodeHyperSwitchMethodDelete(in); return err }
			}
			var err error
			in.Payload, err = json.Marshal(payload)
			require.NoError(t, err)
			for _, key := range []string{kind + ":" + method.String(), manualMethodDeleteKey(kind, method)} {
				in.IdempotencyKey = key
				require.NoError(t, decode(in), "exact accepted target key %q", key)
			}
			for _, key := range []string{"", "unrelated", manualMethodDeleteKey(kind, uuid.New()), "manual:another_type:" + method.String()} {
				in.IdempotencyKey = key
				require.Error(t, decode(in), "must reject unbound key %q", key)
			}
		})
	}
}
