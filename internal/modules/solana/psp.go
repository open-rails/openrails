package solana

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

// Treasury is the ctx merchant's Solana treasury: its active Solana PSP's
// account, where every payment to it lands.
func Treasury(ctx context.Context, database *db.DB) (string, error) {
	if database == nil {
		return "", errors.New("solana: no database")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	scope, ok, err := merchants.Of(database).ActivePSPScope(ctx, tid, string(models.RailSolana))
	if errors.Is(err, merchants.ErrNoActivePSP) || (err == nil && !ok) {
		return "", fmt.Errorf("solana: merchant %s has no active Solana PSP, so no treasury", tid)
	}
	if err != nil {
		return "", fmt.Errorf("solana: load active PSP: %w", err)
	}
	treasury := strings.TrimSpace(scope.AccountID)
	if treasury == "" {
		return "", fmt.Errorf("solana: active PSP %s has no treasury", scope.Key)
	}
	return treasury, nil
}
