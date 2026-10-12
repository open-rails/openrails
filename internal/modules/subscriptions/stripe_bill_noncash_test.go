package subscriptions

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestStripeBillRetainsCreditWhenExtraCashArrives(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	sub := &models.Subscription{ID: uuid.New(), CustomerID: uuid.New(), PspID: uuid.New(), Rail: models.RailStripe, CollectionPolicy: models.CollectionPolicyProvider, RailSubscriptionID: "sub_credit"}
	for _, cash := range []int64{0, 40} {
		t.Run(strconv.FormatInt(cash, 10), func(t *testing.T) {
			invoice := StripeBill{ID: "in_credit", SubscriptionID: sub.RailSubscriptionID, Reason: "subscription_cycle", Currency: "USD", Status: "paid", Total: 100, AmountPaid: cash, PrePaymentCredit: 100 - cash,
				Lines: []StripeBillLine{{ID: "il_credit", Description: "Accepted invoice", Amount: 100, PeriodStart: at, PeriodEnd: at.AddDate(0, 1, 0)}},
			}
			if cash > 0 {
				invoice.Payments = []StripeBillPayment{{ID: "inpay_first", ChargeID: "ch_first", Status: "paid", Currency: "USD", Amount: cash, PaidAt: at}}
			}
			first, _, err := StripeBillObservation(context.Background(), nil, sub, invoice)
			require.NoError(t, err)
			invoice.AmountPaid += 25
			invoice.Payments = append(invoice.Payments, StripeBillPayment{ID: "inpay_extra", ChargeID: "ch_extra", Status: "paid", Currency: "USD", Amount: 25, PaidAt: at.Add(time.Hour)})
			later, receipts, err := StripeBillObservation(context.Background(), nil, sub, invoice)
			require.NoError(t, err)
			require.EqualValues(t, (100-cash)*10_000, first.NonCashAmount)
			require.Equal(t, first.NonCashAmount, later.NonCashAmount, "actual extra cash does not erase applied credit")
			require.Equal(t, "mixed", later.SettlementSource)
			require.EqualValues(t, 25*10_000, receipts[len(receipts)-1].Amount)
		})
	}
}
