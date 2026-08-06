package embedded

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PSPDeclaration identifies a payment-service-provider account without
// configuring credentials. Embedded hosts use this before importing billing
// facts that came from a host-owned or synthetic provider.
type PSPDeclaration struct {
	Key       string
	Rail      string
	AccountID string
}

// DeclarePSP idempotently records a PSP identity for an embedded host. It does
// not write secrets or arm the PSP for checkout; those remain the payment-
// provider configuration boundary's responsibility.
//
// The PSP environment is derived from the engine's credential posture, and
// the row receives the same deterministic natural-key ID as every other PSP
// writer. This is the public precursor to ImportBilling when the declared book
// attributes its rows with a PSPRef.
func (e *Embedded) DeclarePSP(ctx context.Context, merchantID merchant.ID, declaration PSPDeclaration) (uuid.UUID, error) {
	if e == nil || e.app == nil || e.app.Runtime == nil || e.app.Runtime.DB == nil {
		return uuid.Nil, fmt.Errorf("embedded billing: runtime not initialized")
	}
	if merchantID.IsZero() {
		return uuid.Nil, fmt.Errorf("embedded billing: DeclarePSP requires a merchant")
	}

	key := strings.ToLower(strings.TrimSpace(declaration.Key))
	rail := strings.ToLower(strings.TrimSpace(declaration.Rail))
	accountID := strings.TrimSpace(declaration.AccountID)
	switch {
	case key == "":
		return uuid.Nil, fmt.Errorf("embedded billing: DeclarePSP requires a key")
	case rail == "":
		return uuid.Nil, fmt.Errorf("embedded billing: DeclarePSP requires a rail")
	case accountID == "":
		return uuid.Nil, fmt.Errorf("embedded billing: DeclarePSP requires an account ID")
	}

	environment := config.ExpectedProviderEnvironment(e.app.Runtime.Config.IsTestMode())
	database := e.app.Runtime.DB
	if err := merchants.AssertPSPUnowned(
		ctx,
		gen.New(database.DataPool()),
		merchantID.UUID(),
		rail,
		environment,
		accountID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("embedded billing: declare PSP: %w", err)
	}

	pspID, normalizedRail, normalizedEnvironment, normalizedAccountID := merchants.PSPNaturalKey(rail, environment, accountID)
	archived := false
	err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		_, err := database.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
			ID:          pspID,
			MerchantID:  merchantID.UUID(),
			Rail:        normalizedRail,
			Environment: &normalizedEnvironment,
			AccountID:   normalizedAccountID,
			Key:         &key,
			Archived:    &archived,
		})
		return err
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("embedded billing: declare PSP %s: %w", key, err)
	}
	return pspID, nil
}
