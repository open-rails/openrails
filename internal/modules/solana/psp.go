package solana

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

type activeSolanaPSP struct {
	AccountID       string
	RecipientWallet string
}

func resolveActiveSolanaPSP(ctx context.Context, database *db.DB, cfg *config.Config) (activeSolanaPSP, bool, error) {
	if database == nil {
		return activeSolanaPSP{}, false, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return activeSolanaPSP{}, false, nil
	}
	environment := config.ExpectedProviderEnvironment(false)
	if cfg != nil {
		environment = config.ExpectedProviderEnvironment(config.IsTestMode(cfg))
	}

	scope, ok, err := merchants.Of(database).ActivePSPScope(ctx, tid, string(models.RailSolana), environment)
	if errors.Is(err, merchants.ErrNoActivePSP) || (err == nil && !ok) {
		return activeSolanaPSP{}, false, nil
	}
	if err != nil {
		return activeSolanaPSP{}, false, fmt.Errorf("solana: load active PSP: %w", err)
	}
	accountID := strings.TrimSpace(scope.AccountID)
	if accountID == "" {
		return activeSolanaPSP{}, false, fmt.Errorf("solana: active PSP has empty account_id")
	}
	recipient := strings.TrimSpace(fmt.Sprint(scope.Settings["recipient_wallet"]))
	if scope.Settings["recipient_wallet"] == nil || recipient == "" {
		recipient = accountID
	}
	return activeSolanaPSP{AccountID: accountID, RecipientWallet: recipient}, true, nil
}

func ResolveRecipientWallet(ctx context.Context, database *db.DB, cfg *config.Config) (string, error) {
	if account, ok, err := resolveActiveSolanaPSP(ctx, database, cfg); err != nil || ok {
		if err != nil {
			return "", err
		}
		return account.RecipientWallet, nil
	}
	return "", fmt.Errorf("merchant wallet not configured")
}
