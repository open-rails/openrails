package intents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
)

func TestStaffDestructiveIntentRequiresOriginalAdmission(t *testing.T) {
	for _, kind := range []string{TypeNMIRefund, TypeStripeRefund, TypeNMIDeleteSubscription, TypeCCBillCancelSubscription, TypeStripeCancelSubscription, TypeNMIPaymentMethodDelete, TypeHyperSwitchMethodDelete} {
		t.Run(kind, func(t *testing.T) {
			in := gen.BillingProviderIntent{ID: uuid.New(), MerchantID: uuid.New(), IntentType: kind, Origin: string(OriginAdmin)}
			_, err := startProtectedEffect(context.Background(), nil, in, time.Time{})
			require.ErrorContains(t, err, "no admission")
			in.Origin = string(OriginProvider)
			_, err = startProtectedEffect(context.Background(), nil, in, time.Time{})
			require.NoError(t, err, "a verified provider fact retains its separate execution policy")
		})
	}
}
