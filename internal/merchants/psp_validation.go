package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

const providerCredentialProbeTimeout = 15 * time.Second

// probePaymentProviderCredentials checks the PSP's account credentials with
// its provider, read-only. It reports whether a check ran: a rail with no
// check, or a PSP without the credentials one reads, is not probed.
func (s *Service) probePaymentProviderCredentials(ctx context.Context, id billing.MerchantID, p merchantdocs.PSP) (bool, error) {
	secret := func(key string) string { return strings.TrimSpace(p.Secrets[key]) }
	switch p.Rail {
	case "stripe":
		secretKey := secret("secret_key")
		if secretKey == "" {
			return false, nil
		}
		probeCtx, cancel := context.WithTimeout(ctx, providerCredentialProbeTimeout)
		defer cancel()
		if err := stripeAccountCheck(probeCtx, secretKey, p.Environment, p.AccountID, s.StripeClients); err != nil {
			return false, err
		}
		return true, nil
	case "nmi":
		securityKey := secret("security_key")
		if securityKey == "" {
			return false, nil
		}
		deployment, err := config.NMIEndpointDeployment(p.Settings)
		if err != nil {
			return false, err
		}
		client, err := nmi.NewAccountClient(id.UUID(), PspID(p.Rail, p.Environment, p.AccountID), p.AccountID, &config.NMIProviderSettings{SecurityKey: securityKey, EndpointDeployment: deployment}, p.Environment == "test")
		if err != nil {
			return false, fmt.Errorf("merchants: build nmi credential probe: %w", err)
		}
		if s.nmiWire != nil {
			s.nmiWire(client)
		}
		if s.nmiCredentialProbeQueryURL != "" {
			client.QueryURL = s.nmiCredentialProbeQueryURL
		}
		probeCtx, cancel := context.WithTimeout(ctx, providerCredentialProbeTimeout)
		defer cancel()
		if err := client.ProbeCredentials(probeCtx); err != nil {
			return false, providerCredentialError(fmt.Errorf("merchants: validate nmi credentials: %w", err))
		}
		return true, nil
	case "ccbill":
		username, password := secret("datalink_username"), secret("datalink_password")
		if username == "" && password == "" {
			return false, nil
		}
		if username == "" || password == "" {
			return false, apperr.Invalidf("merchants: ccbill datalink_username and datalink_password are required together")
		}
		clientAccNum, clientSubAcc, err := config.SplitCCBillAccountID(p.AccountID)
		if err != nil {
			return false, apperr.Invalidf("merchants: validate ccbill account id: %v", err)
		}
		client := ccbill.NewDataLinkClient(&config.CCBillConfig{
			ClientAccNum:     clientAccNum,
			ClientSubAcc:     clientSubAcc,
			DataLinkUsername: username,
			DataLinkPassword: password,
			TestMode:         p.Environment == "test",
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

// validateCredentialValue applies a rail's format rules to one credential.
func validateCredentialValue(rail, key, value string) error {
	if value == "" {
		return errors.New("empty")
	}
	if rail == "stripe" {
		switch key {
		case "secret_key":
			if !strings.HasPrefix(value, "sk_") && !strings.HasPrefix(value, "rk_") {
				return errors.New("invalid_format")
			}
		case "webhook_signing_secret", "webhook_signing_secret_thin", "webhook_signing_secret_previous":
			if !strings.HasPrefix(value, "whsec_") {
				return errors.New("invalid_format")
			}
		}
	}
	return nil
}
