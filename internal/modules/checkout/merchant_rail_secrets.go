package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
)

type merchantSecretGetter interface {
	Get(ctx context.Context, merchantID merchant.ID, name string) (merchants.Secret, error)
}

type providerAccountSecretResolver interface {
	PrimaryProviderAccountSecretName(ctx context.Context, merchantID merchant.ID, providerType, environment, key string) (string, bool, error)
}

// SetMerchantSecretStore wires the dynamic OpenRails merchant-secret store into
// checkout money paths. Static rail config remains available only when no
// provider-account resolver is configured; once scoped provider accounts are in
// use, missing scoped secrets fail closed instead of falling back across
// accounts.
func (s *CheckoutService) SetMerchantSecretStore(store merchantSecretGetter) {
	if s == nil {
		return
	}
	s.MerchantSecrets = store
	if s.NMISaleService != nil {
		s.NMISaleService.ResolveNMIClient = s.resolveNMIClient
	}
}

func (s *CheckoutService) SetProviderAccountSecretResolver(resolver providerAccountSecretResolver) {
	if s == nil {
		return
	}
	s.ProviderSecrets = resolver
}

func (s *CheckoutService) merchantSecret(ctx context.Context, name string) (string, bool, error) {
	if s == nil || s.MerchantSecrets == nil {
		return "", false, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", false, err
	}
	sec, err := s.MerchantSecrets.Get(ctx, tid, name)
	if err != nil {
		if errors.Is(err, merchants.ErrSecretNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	value := strings.TrimSpace(sec.Value)
	if value == "" {
		return "", false, nil
	}
	return value, true, nil
}

func (s *CheckoutService) merchantProviderSecret(ctx context.Context, providerType, environment, key string) (string, bool, error) {
	if s == nil || s.MerchantSecrets == nil || s.ProviderSecrets == nil {
		return "", false, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", false, err
	}
	name, ok, err := s.ProviderSecrets.PrimaryProviderAccountSecretName(ctx, tid, providerType, environment, key)
	if err != nil || !ok {
		return "", ok, err
	}
	return s.merchantSecret(ctx, name)
}

func (s *CheckoutService) scopedProviderSecretsEnabled() bool {
	return s != nil && s.MerchantSecrets != nil && s.ProviderSecrets != nil
}

func (s *CheckoutService) resolveNMIClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, errors.New("provider is required")
	}

	secretKey := ""
	if provider == "mobius" {
		secretKey = "production_key"
	}
	if secretKey != "" {
		if value, ok, err := s.merchantProviderSecret(ctx, config.RailTypeNMI, "live", secretKey); err != nil {
			return nil, fmt.Errorf("load merchant NMI secret: %w", err)
		} else if ok {
			proc := cloneRailConfig(s.railConfig(provider))
			if proc == nil {
				proc = &config.RailConfig{Type: config.RailTypeNMI}
			}
			proc.Type = config.RailTypeNMI
			proc.SecurityKey = value
			return nmi.NewClient(provider, proc.ToNMIProviderSettings(provider), s.Config != nil && s.Config.IsTestMode())
		} else if s.scopedProviderSecretsEnabled() {
			return nil, fmt.Errorf("missing scoped merchant NMI secret for provider account")
		}
	}

	if s != nil && s.NMIClients != nil {
		if client := s.NMIClients[provider]; client != nil {
			return client, nil
		}
	}
	if proc := s.railConfig(provider); proc != nil && rails.IsNMIBacked(provider) {
		return nmi.NewClient(provider, proc.ToNMIProviderSettings(provider), s.Config != nil && s.Config.IsTestMode())
	}
	return nil, fmt.Errorf("missing client")
}

func (s *CheckoutService) resolveCCBillClient(ctx context.Context) (*ccbill.CCBillClient, error) {
	cfg, err := s.resolveCCBillConfig(ctx)
	if err != nil {
		return nil, err
	}
	return ccbill.NewClient(cfg, s.Config != nil && s.Config.IsTestMode()), nil
}

func (s *CheckoutService) resolveCCBillConfig(ctx context.Context) (*config.CCBillConfig, error) {
	baseProc := s.railConfig("ccbill")
	var base *config.CCBillConfig
	if baseProc != nil {
		base = baseProc.ToCCBillConfig()
	} else {
		base = &config.CCBillConfig{}
	}

	value, ok, err := s.merchantProviderSecret(ctx, config.RailTypeCCBill, "live", "account_config")
	if err != nil {
		return nil, fmt.Errorf("load merchant CCBill secret: %w", err)
	}
	if ok {
		cfg, err := parseMerchantCCBillConfig(value, base)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if s.scopedProviderSecretsEnabled() {
		return nil, errors.New("missing scoped merchant CCBill account_config secret for provider account")
	}

	if baseProc == nil {
		return nil, errors.New("ccbill rail config is required")
	}
	return base, nil
}

func (s *CheckoutService) railConfig(name string) *config.RailConfig {
	if s == nil || s.Rails == nil {
		return nil
	}
	return s.Rails.GetRail(name)
}

func cloneRailConfig(in *config.RailConfig) *config.RailConfig {
	if in == nil {
		return nil
	}
	out := *in
	if in.Tokens != nil {
		out.Tokens = make(map[string]config.TokenConfig, len(in.Tokens))
		for k, v := range in.Tokens {
			out.Tokens[k] = v
		}
	}
	return &out
}

func parseMerchantCCBillConfig(raw string, base *config.CCBillConfig) (*config.CCBillConfig, error) {
	cfg := &config.CCBillConfig{}
	if base != nil {
		*cfg = *base
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("parse merchant CCBill account config: %w", err)
	}
	set := func(dst *string, keys ...string) {
		for _, key := range keys {
			if v, ok := data[key]; ok {
				if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
					*dst = s
					return
				}
			}
		}
	}
	set(&cfg.ClientAccNum, "client_acc_num", "clientAccNum", "ClientAccNum")
	set(&cfg.ClientSubAcc, "client_sub_acc", "clientSubAcc", "ClientSubAcc")
	set(&cfg.Salt, "salt", "Salt")
	set(&cfg.DataLinkUsername, "datalink_username", "dataLinkUsername", "DataLinkUsername")
	set(&cfg.DataLinkPassword, "datalink_password", "dataLinkPassword", "DataLinkPassword")

	if strings.TrimSpace(cfg.ClientAccNum) == "" || strings.TrimSpace(cfg.ClientSubAcc) == "" {
		return nil, errors.New("merchant CCBill account config requires client_acc_num and client_sub_acc")
	}
	return cfg, nil
}
