package checkout

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCompensateFailedUpgradeCancelsNewSubscription(t *testing.T) {
	svc := &CheckoutService{} // no NMI clients configured
	rolledBack := false
	oldID := uuid.New()

	// prorationTransactionID set but no client configured: the refund path logs an
	// error and the new-subscription rollback must still run. Must not panic.
	svc.compensateFailedUpgrade(
		context.Background(),
		"mobius",
		"txn-123",
		uuid.New(),
		"nmi-sub-1",
		"user-1",
		&oldID,
		func() { rolledBack = true },
		errors.New("db write failed"),
	)

	if !rolledBack {
		t.Fatal("expected new NMI subscription rollback to be invoked during compensation")
	}
}

func TestCompensateFailedUpgradeNoProrationStillRollsBack(t *testing.T) {
	svc := &CheckoutService{}
	rolledBack := false

	// No proration charge (downgrade-style zero) — rollback still runs.
	svc.compensateFailedUpgrade(
		context.Background(),
		"mobius",
		"",
		uuid.New(),
		"nmi-sub-2",
		"user-2",
		nil,
		func() { rolledBack = true },
		errors.New("db write failed"),
	)

	if !rolledBack {
		t.Fatal("expected rollback even when there is no proration to refund")
	}
}
