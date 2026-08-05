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
		environment = config.ExpectedProviderEnvironment(cfg.IsTestMode())
	}

	var row gen.OpenrailsPsp
	if err := database.RunInMerchantConn(merchant.WithID(ctx, tid), func(ctx context.Context) error {
		var qerr error
		row, qerr = database.Gen(ctx).GetActivePSPForNewWork(ctx, gen.GetActivePSPForNewWorkParams{
			MerchantID:  tid.UUID(),
			Rail:        string(models.RailSolana),
			Environment: &environment,
		})
		return qerr
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return activeSolanaPSP{}, false, nil
		}
		return activeSolanaPSP{}, false, fmt.Errorf("solana: load active PSP: %w", err)
	}

	accountID := strings.TrimSpace(row.AccountID)
	if accountID == "" {
		return activeSolanaPSP{}, false, fmt.Errorf("solana: active PSP has empty account_id")
	}
	recipient := strings.TrimSpace(solanaPSPSettings(row.Evidence)["recipient_wallet"])
	if recipient == "" {
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

func solanaPSPSettings(raw []byte) map[string]string {
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
