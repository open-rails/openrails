package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

const providerCredentialProbeTimeout = 15 * time.Second

func (s *Service) probePaymentProviderCredentials(ctx context.Context, id merchant.ID, rail, environment, accountID string, supplied map[string]string) (bool, error) {
	switch rail {
	case "stripe":
		secretKey, ok, err := s.effectiveProviderCredential(ctx, id, rail, environment, accountID, supplied, "secret_key")
		if err != nil || !ok {
			return false, err
		}
		probeCtx, cancel := context.WithTimeout(ctx, providerCredentialProbeTimeout)
		defer cancel()
		if err := stripeAccountCheck(probeCtx, secretKey, environment, accountID, s.StripeClients); err != nil {
			return false, err
		}
		return true, nil
	case "nmi":
		securityKey, ok, err := s.effectiveProviderCredential(ctx, id, rail, environment, accountID, supplied, "security_key")
		if err != nil || !ok {
			return false, err
		}
		deployment, pspID, err := s.storedNMIDeployment(ctx, id, rail, environment, accountID)
		if err != nil {
			return false, err
		}
		client, err := nmi.NewAccountClient(id.UUID(), pspID, accountID, &config.NMIProviderSettings{SecurityKey: securityKey, EndpointDeployment: deployment}, environment == "test")
		if err != nil {
			return false, fmt.Errorf("merchants: build nmi credential probe: %w", err)
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
			return false, apperr.Invalidf("merchants: ccbill datalink_username and datalink_password are required together")
		}
		clientAccNum, clientSubAcc, err := config.SplitCCBillAccountID(accountID)
		if err != nil {
			return false, apperr.Invalidf("merchants: validate ccbill account id: %v", err)
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
	secret, err := s.readPublishedProviderCredential(ctx, id, rail, environment, accountID, key, name)
	if errors.Is(err, ErrSecretNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("merchants: load provider credential %q: %w", key, err)
	}
	value := strings.TrimSpace(secret.Value)
	return value, value != "", nil
}

func (s *Service) readPublishedProviderCredential(ctx context.Context, id merchant.ID, rail, environment, account, key, name string) (Secret, error) {
	ref := SecretRef{Name: name}
	if s.pool != nil {
		var row gen.OpenrailsPsp
		err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			row, err = gen.New(tx).GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{MerchantID: id.UUID(), Rail: rail, Environment: &environment, AccountID: account})
			return err
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Secret{}, err
		}
		if err == nil {
			ref, err = PSPSecretRef(rail, environment, account, row.Evidence, key)
			if err != nil {
				return Secret{}, err
			}
		}
	}
	return ReadSecretRef(ctx, s.secrets, id, ref)
}
