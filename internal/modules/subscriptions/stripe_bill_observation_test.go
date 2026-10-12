package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestStripeBillSettlementEvidence(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	sub := &models.Subscription{ID: uuid.New(), CustomerID: uuid.New(), PspID: uuid.New(), Rail: models.RailStripe, CollectionPolicy: models.CollectionPolicyProvider, RailSubscriptionID: "sub_verified"}
	quantity := 3
	base := StripeBill{ID: "in_verified", SubscriptionID: sub.RailSubscriptionID, Reason: "subscription_cycle", Currency: "USD", Status: "open", Total: 1000, AmountDue: 1000, AmountRemaining: 1000, Lines: []StripeBillLine{{ID: "il_verified", Description: "Accepted provider charge", Quantity: &quantity, Amount: 1000, PeriodStart: at, PeriodEnd: at.AddDate(0, 1, 0)}}}
	pay := func(id string, amount int64) StripeBillPayment {
		return StripeBillPayment{ID: "inpay_" + id, ChargeID: "ch_" + id, Status: "paid", Currency: "USD", Amount: amount, PaidAt: at}
	}
	for _, name := range []string{"partial", "two payments", "credit", "mixed", "out of band", "zero", "unexplained paid", "wrong currency", "duplicate capture"} {
		t.Run(name, func(t *testing.T) {
			in := base
			in.Status = "paid"
			in.AmountRemaining = 0
			in.AmountPaid = 1000
			expectedSource := "cash"
			expectedPayments := 0
			bad := false
			switch name {
			case "partial":
				in.Status = "open"
				in.AmountPaid = 400
				in.AmountRemaining = 600
				in.Payments = []StripeBillPayment{pay("a", 400)}
				expectedPayments = 1
				expectedSource = ""
			case "two payments":
				in.Payments = []StripeBillPayment{pay("a", 400), pay("b", 600)}
				expectedPayments = 2
			case "credit":
				in.AmountDue = 0
				in.AmountPaid = 0
				in.StartingBalance = -1000
				expectedSource = "credit"
			case "mixed":
				in.AmountPaid = 400
				in.Payments = []StripeBillPayment{pay("a", 400)}
				in.PrePaymentCredit = 600
				expectedPayments = 1
				expectedSource = "mixed"
			case "out of band":
				in.AmountOffStripe = 1000
				expectedSource = "out_of_band"
			case "zero":
				in.Total = 0
				in.AmountDue = 0
				in.AmountPaid = 0
				in.Lines = append([]StripeBillLine(nil), base.Lines...)
				in.Lines[0].Amount = 0
			case "unexplained paid":
				bad = true
			case "wrong currency":
				in.Payments = []StripeBillPayment{pay("a", 1000)}
				in.Payments[0].Currency = "EUR"
				bad = true
			case "duplicate capture":
				in.Payments = []StripeBillPayment{pay("a", 500), pay("a", 500)}
				bad = true
			}
			bill, payments, err := StripeBillObservation(context.Background(), nil, sub, in)
			if bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, payments, expectedPayments)
			require.Equal(t, expectedSource, bill.SettlementSource)
			require.Equal(t, name != "partial", bill.Settled)
			require.Equal(t, in.Total*10_000, bill.Total)
			require.Equal(t, in.Lines[0].Amount*10_000, bill.Lines[0].Amount, "non-divisible seat total is preserved")
			if name == "partial" {
				require.Zero(t, bill.NonCashAmount)
			}
		})
	}
}
