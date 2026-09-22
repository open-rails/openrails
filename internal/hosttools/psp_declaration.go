package hosttools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PSPDeclaration identifies a payment-service-provider account without
// configuring credentials. Hosts use this only for imported facts from a host-owned or synthetic provider.
// Existing aliases, archive state, custody and configuration are preserved.
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
func DeclarePSP(ctx context.Context, application *app.App, merchantID merchant.ID, declaration PSPDeclaration) (uuid.UUID, error) {
	if application == nil || application.Runtime == nil || application.Runtime.DB == nil {
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

	environment := config.ExpectedProviderEnvironment(application.Runtime.Config.IsTestMode())
	database := application.Runtime.DB
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
	var declaredID uuid.UUID
	err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		var err error
		declaredID, err = database.Gen(ctx).DeclarePSPIdentity(ctx, gen.DeclarePSPIdentityParams{
			ID: pspID, MerchantID: merchantID.UUID(), Rail: normalizedRail,
			Environment: normalizedEnvironment, AccountID: normalizedAccountID, Key: key,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = openrails.ErrConflict
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("embedded billing: declare PSP %s: %w", key, err)
	}
	return declaredID, nil
}
