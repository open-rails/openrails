package merchants

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
		client, err := nmi.NewAccountClient(s.Log, id.UUID(), PspID(p.Rail, p.Environment, p.AccountID), p.AccountID, &config.NMIProviderSettings{SecurityKey: securityKey, Environment: p.Environment})
		if err != nil {
			return false, fmt.Errorf("merchants: build nmi credential probe: %w", err)
		}
		if s.nmiWire != nil {
			s.nmiWire(client)
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
		client := ccbill.NewDataLinkClient(s.Log, &config.CCBillConfig{
			ClientAccNum:     clientAccNum,
			ClientSubAcc:     clientSubAcc,
			DataLinkUsername: username,
			DataLinkPassword: password,
			Sandbox:          p.Environment == config.EnvironmentSandbox,
		})
		if s.dataLinkURL != "" {
			client.BaseURL, client.LoopbackFixture = s.dataLinkURL, true
		}
		client.Pacing = s.dataLinkPacing
		probeCtx, cancel := context.WithTimeout(ctx, providerCredentialProbeTimeout)
		defer cancel()
		if err := client.ProbeCredentials(probeCtx); err != nil {
			var limited *ccbill.ProbeLimitedError
			if errors.As(err, &limited) {
				return false, &ProbeLimitedError{RetryAfter: max(limited.Wait, time.Second)}
			}
			if errors.Is(err, ccbill.ErrDataLinkIndeterminate) {
				return false, apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable,
					"CCBill could not complete the credential check (results=-7); try again")
			}
			return false, providerCredentialError(fmt.Errorf("merchants: validate ccbill credentials: %w", err))
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
