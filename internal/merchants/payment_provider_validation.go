package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/pkg/merchant"
)

const providerCredentialProbeTimeout = 15 * time.Second

func (s *Service) probePaymentProviderCredentials(ctx context.Context, id merchant.ID, rail, environment, accountID string, supplied map[string]string) (bool, error) {
	switch rail {
	case "nmi":
		securityKey, ok, err := s.effectiveProviderCredential(ctx, id, rail, environment, accountID, supplied, "security_key")
		if err != nil || !ok {
			return false, err
		}
		client, err := nmi.NewClient(accountID, &config.NMIProviderSettings{SecurityKey: securityKey}, environment == "test")
		if err != nil {
			return false, fmt.Errorf("merchants: build nmi credential probe: %w", err)
		}
		if s.nmiCredentialProbeQueryURL != "" {
			client.QueryURL = s.nmiCredentialProbeQueryURL
		}
		probeCtx, cancel := context.WithTimeout(ctx, providerCredentialProbeTimeout)
		defer cancel()
		if err := client.ProbeCredentials(probeCtx); err != nil {
			return false, fmt.Errorf("merchants: validate nmi credentials: %w", err)
		}
		return true, nil

	case "ccbill":
		username, hasUsername, err := s.effectiveProviderCredential(ctx, id, rail, environment, accountID, supplied, "datalink_username")
		if err != nil {
			return false, err
		}
		password, hasPassword, err := s.effectiveProviderCredential(ctx, id, rail, environment, accountID, supplied, "datalink_password")
		if err != nil {
			return false, err
		}
		if !hasUsername && !hasPassword {
			return false, nil
		}
		if !hasUsername || !hasPassword {
			return false, errors.New("merchants: ccbill datalink_username and datalink_password are required together")
		}
		clientAccNum, clientSubAcc, err := config.SplitCCBillAccountID(accountID)
		if err != nil {
			return false, fmt.Errorf("merchants: validate ccbill account id: %w", err)
		}
		client := ccbill.NewDataLinkClient(&config.CCBillConfig{
			ClientAccNum:     clientAccNum,
			ClientSubAcc:     clientSubAcc,
			DataLinkUsername: username,
			DataLinkPassword: password,
			TestMode:         environment == "test",
		})
		if s.ccbillCredentialProbeBaseURL != "" {
			client.BaseURL = s.ccbillCredentialProbeBaseURL
		}
		probeCtx, cancel := context.WithTimeout(ctx, providerCredentialProbeTimeout)
		defer cancel()
		if err := client.ProbeCredentials(probeCtx); err != nil {
			return false, fmt.Errorf("merchants: validate ccbill credentials: %w", err)
		}
		return true, nil
	}
	return false, nil
}

func (s *Service) effectiveProviderCredential(ctx context.Context, id merchant.ID, rail, environment, accountID string, supplied map[string]string, key string) (string, bool, error) {
	if value := strings.TrimSpace(supplied[key]); value != "" {
		return value, true, nil
	}
	name, err := PSPSecretName(rail, environment, accountID, key)
	if err != nil {
		return "", false, err
	}
	secret, err := s.secrets.Get(ctx, id, name)
	if errors.Is(err, ErrSecretNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("merchants: load provider credential %q: %w", key, err)
	}
	value := strings.TrimSpace(secret.Value)
	return value, value != "", nil
}
