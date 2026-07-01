package solana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

type activeSolanaProviderAccount struct {
	AccountID       string
	RecipientWallet string
}

func resolveActiveSolanaProviderAccount(ctx context.Context, database *db.DB, cfg *config.Config) (activeSolanaProviderAccount, bool, error) {
	if database == nil {
		return activeSolanaProviderAccount{}, false, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return activeSolanaProviderAccount{}, false, nil
	}
	environment := config.ExpectedProviderEnvironment(false)
	if cfg != nil {
		environment = config.ExpectedProviderEnvironment(cfg.IsTestMode())
	}

	var row gen.OpenrailsPaymentProviderAccount
	if err := database.RunInMerchantConn(merchant.WithID(ctx, tid), func(ctx context.Context) error {
		var qerr error
		row, qerr = database.Gen(ctx).GetActiveProviderAccountForNewWork(ctx, gen.GetActiveProviderAccountForNewWorkParams{
			MerchantID:  tid.UUID(),
			Rail:        string(models.RailSolana),
			Environment: &environment,
		})
		return qerr
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return activeSolanaProviderAccount{}, false, nil
		}
		return activeSolanaProviderAccount{}, false, fmt.Errorf("solana: load active provider account: %w", err)
	}

	accountID := strings.TrimSpace(row.AccountID)
	if accountID == "" {
		return activeSolanaProviderAccount{}, false, fmt.Errorf("solana: active provider account has empty account_id")
	}
	recipient := strings.TrimSpace(solanaProviderAccountSettings(row.Evidence)["recipient_wallet"])
	if recipient == "" {
		recipient = accountID
	}
	return activeSolanaProviderAccount{AccountID: accountID, RecipientWallet: recipient}, true, nil
}

func ResolveRecipientWallet(ctx context.Context, database *db.DB, cfg *config.Config) (string, error) {
	if account, ok, err := resolveActiveSolanaProviderAccount(ctx, database, cfg); err != nil || ok {
		if err != nil {
			return "", err
		}
		return account.RecipientWallet, nil
	}
	return "", fmt.Errorf("merchant wallet not configured")
}

func solanaProviderAccountSettings(raw []byte) map[string]string {
	var evidence struct {
		Settings map[string]any `json:"settings"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &evidence) != nil || len(evidence.Settings) == 0 {
		return nil
	}
	out := make(map[string]string, len(evidence.Settings))
	for key, value := range evidence.Settings {
		if s := strings.TrimSpace(fmt.Sprint(value)); s != "" {
			out[key] = s
		}
	}
	return out
}
