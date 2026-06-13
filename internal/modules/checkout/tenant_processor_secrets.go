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
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
)

type tenantSecretGetter interface {
	Get(ctx context.Context, tenantID tenant.ID, name string) (tenancy.Secret, error)
}

// SetTenantSecretStore wires the dynamic OpenRails tenant-secret store into the
// checkout money paths. Static processor config remains the fallback for
// embedded/self-hosted installs; tenant secrets take precedence when present.
func (s *CheckoutService) SetTenantSecretStore(store tenantSecretGetter) {
	if s == nil {
		return
	}
	s.TenantSecrets = store
	if s.NMISaleService != nil {
		s.NMISaleService.ResolveNMIClient = s.resolveNMIClient
	}
}

func (s *CheckoutService) tenantSecret(ctx context.Context, name string) (string, bool, error) {
	if s == nil || s.TenantSecrets == nil {
		return "", false, nil
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return "", false, err
	}
	sec, err := s.TenantSecrets.Get(ctx, tid, name)
	if err != nil {
		if errors.Is(err, tenancy.ErrSecretNotFound) {
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

func (s *CheckoutService) resolveNMIClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, errors.New("provider is required")
	}

	secretName := ""
	if provider == "mobius" {
		secretName = tenancy.SecretNMIMobiusProductionKey
	}
	if secretName != "" {
		if value, ok, err := s.tenantSecret(ctx, secretName); err != nil {
			return nil, fmt.Errorf("load tenant NMI secret: %w", err)
		} else if ok {
			proc := cloneProcessorConfig(s.processorConfig(provider))
			if proc == nil {
				proc = &config.ProcessorConfig{Type: config.ProcessorTypeNMI}
			}
			proc.Type = config.ProcessorTypeNMI
			proc.SecurityKey = value
			return nmi.NewClient(provider, proc.ToNMIProviderSettings(provider), s.Config != nil && s.Config.IsTestEnv())
		}
	}

	if s != nil && s.NMIClients != nil {
		if client := s.NMIClients[provider]; client != nil {
			return client, nil
		}
	}
	if proc := s.processorConfig(provider); proc != nil && processors.IsNMIBacked(provider) {
		return nmi.NewClient(provider, proc.ToNMIProviderSettings(provider), s.Config != nil && s.Config.IsTestEnv())
	}
	return nil, fmt.Errorf("missing client")
}

func (s *CheckoutService) resolveCCBillClient(ctx context.Context) (*ccbill.CCBillClient, error) {
	cfg, err := s.resolveCCBillConfig(ctx)
	if err != nil {
		return nil, err
	}
	return ccbill.NewClient(cfg, s.Config != nil && s.Config.IsTestEnv()), nil
}

func (s *CheckoutService) resolveCCBillConfig(ctx context.Context) (*config.CCBillConfig, error) {
	baseProc := s.processorConfig("ccbill")
	var base *config.CCBillConfig
	if baseProc != nil {
		base = baseProc.ToCCBillConfig()
	} else {
		base = &config.CCBillConfig{}
	}

	value, ok, err := s.tenantSecret(ctx, tenancy.SecretCCBillAccountConfig)
	if err != nil {
		return nil, fmt.Errorf("load tenant CCBill secret: %w", err)
	}
	if ok {
		cfg, err := parseTenantCCBillConfig(value, base)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	if baseProc == nil {
		return nil, errors.New("ccbill processor config is required")
	}
	return base, nil
}

func (s *CheckoutService) processorConfig(name string) *config.ProcessorConfig {
	if s == nil || s.Config == nil {
		return nil
	}
	return s.Config.GetProcessor(name)
}

func cloneProcessorConfig(in *config.ProcessorConfig) *config.ProcessorConfig {
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

func parseTenantCCBillConfig(raw string, base *config.CCBillConfig) (*config.CCBillConfig, error) {
	cfg := &config.CCBillConfig{}
	if base != nil {
		*cfg = *base
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("parse tenant CCBill account config: %w", err)
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
	set(&cfg.SubscriptionTypeId, "subscription_type_id", "subscriptionTypeId", "SubscriptionTypeId")
	set(&cfg.DataLinkUsername, "datalink_username", "dataLinkUsername", "DataLinkUsername")
	set(&cfg.DataLinkPassword, "datalink_password", "dataLinkPassword", "DataLinkPassword")

	if strings.TrimSpace(cfg.ClientAccNum) == "" || strings.TrimSpace(cfg.ClientSubAcc) == "" {
		return nil, errors.New("tenant CCBill account config requires client_acc_num and client_sub_acc")
	}
	return cfg, nil
}
