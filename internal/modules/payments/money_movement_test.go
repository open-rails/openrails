package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// or#827: the settlement feed publishes on money_movement alone, so the one
// place every payment row is minted refuses to guess it. The DB default is
// fail-closed ('none'); this is the other half — a real charge that forgets to
// declare must not silently disappear from the feed either.
func TestResolveMoneyMovementRequiresDeclarationOnSettlementCandidates(t *testing.T) {
	base := func() *models.Payment {
		return &models.Payment{ID: uuid.New(), Amount: 7_000_000, Currency: "usd", TransactionID: "txn"}
	}

	// Undeclared + completed + positive + not a reversal = refused.
	p := base()
	p.Status = "completed"
	_, err := resolveMoneyMovement(p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "money_movement must be declared")

	// An empty status IS 'completed' (the column default), so it counts too.
	p = base()
	_, err = resolveMoneyMovement(p)
	require.Error(t, err, "an empty status defaults to completed in SQL and must not slip past")

	// Declared either way, it is taken verbatim.
	for _, want := range []models.MoneyMovement{models.MoneyMovementRail, models.MoneyMovementNone} {
		p = base()
		p.Status = "completed"
		p.MoneyMovement = want
		got, err := resolveMoneyMovement(p)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	// An invented value never reaches the CHECK constraint.
	p = base()
	p.MoneyMovement = models.MoneyMovement("maybe")
	_, err = resolveMoneyMovement(p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a known value")

	// Rows that can never settle may stay undeclared: they resolve to 'none'.
	refundedID := uuid.New()
	for name, mutate := range map[string]func(*models.Payment){
		"declined":    func(p *models.Payment) { p.Status = "failed" },
		"pending":     func(p *models.Payment) { p.Status = "pending" },
		"reversal":    func(p *models.Payment) { p.Amount, p.RefundedPaymentID = -7_000_000, &refundedID },
		"zero amount": func(p *models.Payment) { p.Amount = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			p := base()
			mutate(p)
			got, err := resolveMoneyMovement(p)
			require.NoError(t, err)
			require.Equal(t, models.MoneyMovementNone, got)
		})
	}
}

// A payment that records no money movement cannot be refunded — you can only
// send back money that arrived. This replaced a transaction_id prefix denylist.
func TestValidateRefundRequiresMoneyMovement(t *testing.T) {
	svc := &PaymentService{}
	p := &models.Payment{
		ID:            uuid.New(),
		Amount:        1000,
		Status:        "completed",
		TransactionID: "nmi_sub_attempt:order-1",
		MoneyMovement: models.MoneyMovementNone,
	}
	err := svc.ValidateRefund(context.Background(), p, 500)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no money movement")
}
