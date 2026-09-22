package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Browser keys are public operator declarations on the SAME immutable PSP as
// the setup/payment. Never infer a key from another active/default account.
func stripeBrowserKey(account gen.OpenrailsPsp) (string, error) {
	var evidence struct {
		Settings struct {
			PublishableKey string `json:"publishable_key"`
		} `json:"settings"`
		PublicConfig struct {
			PublishableKey string `json:"publishable_key"`
		} `json:"public_config"`
	}
	if json.Unmarshal(account.Evidence, &evidence) != nil {
		return "", errors.New("Stripe browser configuration is unavailable")
	}
	key := evidence.PublicConfig.PublishableKey
	if declared := evidence.Settings.PublishableKey; declared != "" {
		if key != "" && key != declared {
			return "", errors.New("Stripe browser key declarations conflict")
		}
		key = declared
	}
	prefix := "pk_live_"
	if account.Environment == "test" {
		prefix = "pk_test_"
	}
	if (account.Environment != "test" && account.Environment != "live") || account.Rail != "stripe" || len(key) <= len(prefix) || len(key) > 255 || strings.TrimSpace(key) != key || !strings.HasPrefix(key, prefix) {
		return "", errors.New("Stripe account requires its matching publishable key")
	}
	for _, ch := range key {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
			return "", errors.New("Stripe publishable key is invalid")
		}
	}
	return key, nil
}
func (s *CheckoutService) stripeBrowserKey(ctx context.Context, psp uuid.UUID) (string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	account, err := s.SubscriptionService.Database().Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: psp})
	if err != nil {
		return "", err
	}
	return stripeBrowserKey(account)
}
