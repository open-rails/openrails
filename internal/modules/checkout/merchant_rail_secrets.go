package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// SetMerchantSecretStore wires the dynamic OpenRails merchant-secret store into
// checkout money paths. Static rail config remains available only when no
// provider-account resolver is configured; once scoped provider accounts are in
// use, missing scoped secrets fail closed instead of falling back across
// accounts.
func (s *CheckoutService) SetMerchantSecretStore(store merchants.MerchantSecretReader) {
	if s == nil {
		return
	}
	s.MerchantSecrets = store
	if s.NMISaleService != nil {
		s.NMISaleService.ResolveNMIClient = s.resolveNMIClient
	}
}

func (s *CheckoutService) SetRailMerchantAccountSecretResolver(resolver merchants.RailMerchantAccountSecretResolver) {
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

func (s *CheckoutService) merchantProviderSecret(ctx context.Context, rail, environment, key string) (string, bool, error) {
	if s == nil || s.MerchantSecrets == nil || s.ProviderSecrets == nil {
		return "", false, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", false, err
	}
	name, ok, err := s.ProviderSecrets.ActiveRailMerchantAccountSecretName(ctx, tid, rail, environment, key)
	if err != nil || !ok {
		return "", ok, err
	}
	return s.merchantSecret(ctx, name)
}

func (s *CheckoutService) scopedProviderSecretsEnabled() bool {
	return s != nil && s.MerchantSecrets != nil && s.ProviderSecrets != nil
}

// railMerchantAccountEnvironment is the environment provider-account rows carry in
// this deployment: test under test_mode, live otherwise (#641).
func (s *CheckoutService) railMerchantAccountEnvironment() string {
	return config.ExpectedProviderEnvironment(s != nil && s.Config != nil && s.Config.IsTestMode())
}

// ResolveRailMerchantAccountID resolves the ACTIVE provider account for new work
// on the given rail (#704). Returns nil when no resolver is wired or no active
// account exists — provenance is only ever stamped with a REAL resolved account,
// never invented. Resolution failures are logged and swallowed: stamping is
// metadata and must not block a sale.
func (s *CheckoutService) ResolveRailMerchantAccountID(ctx context.Context, rail string) *uuid.UUID {
	if s == nil || s.ProviderSecrets == nil {
		return nil
	}
	scopes, ok := s.ProviderSecrets.(merchants.RailMerchantAccountScopeResolver)
	if !ok {
		return nil
	}
	rail = strings.ToLower(strings.TrimSpace(rail))
	if rail == "" {
		return nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil
	}
	scope, found, err := scopes.ActiveRailMerchantAccountScope(ctx, tid, rail, s.railMerchantAccountEnvironment())
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("rail", rail).Debug("checkout: provider-account resolution failed; leaving provenance unstamped")
		return nil
	}
	if !found || scope.ID == uuid.Nil {
		return nil
	}
	id := scope.ID
	return &id
}

// stampRailMerchantAccount pins resolved account provenance into ctx so the
// payment / subscription / payment-method writes downstream of this checkout
// flow stamp rail_merchant_account_id (#704).
func (s *CheckoutService) stampRailMerchantAccount(ctx context.Context, rail string) context.Context {
	if id := s.ResolveRailMerchantAccountID(ctx, rail); id != nil {
		return db.WithRailMerchantAccountID(ctx, *id)
	}
	return ctx
}

func (s *CheckoutService) resolveNMIClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, errors.New("rail is required")
	}

	if rails.IsNMI(models.Rail(provider)) {
		// Environment follows test_mode (#681), same as the CCBill leg.
		if value, ok, err := s.merchantProviderSecret(ctx, string(models.RailNMI), s.railMerchantAccountEnvironment(), "security_key"); err != nil {
			return nil, fmt.Errorf("load merchant NMI secret: %w", err)
		} else if ok {
			proc := cloneRailConfig(s.activeNMIConfig())
			if proc == nil {
				proc = &config.RailMerchantAccountConfig{Rail: models.RailNMI, NMI: &config.NMIRailConfig{}}
			}
			proc.Rail = models.RailNMI
			if proc.NMI == nil {
				proc.NMI = &config.NMIRailConfig{}
			}
			proc.NMI.SecurityKey = value
			return nmi.NewClient(provider, proc.ToNMIProviderSettings(), s.Config != nil && s.Config.IsTestMode())
		} else if s.scopedProviderSecretsEnabled() {
			return nil, fmt.Errorf("missing scoped merchant NMI secret for provider account")
		}
	}

	if s != nil && s.NMIClients != nil {
		if client := s.NMIClients[provider]; client != nil {
			return client, nil
		}
	}
	if rails.IsNMI(models.Rail(provider)) {
		if proc := s.activeNMIConfig(); proc != nil {
			return nmi.NewClient(provider, proc.ToNMIProviderSettings(), s.Config != nil && s.Config.IsTestMode())
		}
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

	if s.scopedProviderSecretsEnabled() {
		cfg, err := s.resolveScopedCCBillConfig(ctx, base)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	if baseProc == nil {
		return nil, errors.New("ccbill rail config is required")
	}
	return base, nil
}

func (s *CheckoutService) resolveScopedCCBillConfig(ctx context.Context, base *config.CCBillConfig) (*config.CCBillConfig, error) {
	scopeResolver, ok := s.ProviderSecrets.(merchants.RailMerchantAccountScopeResolver)
	if !ok {
		return nil, errors.New("missing scoped merchant CCBill provider account resolver")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// Environment follows test_mode (#641/#668): sandbox deployments declare
	// environment=test rows (ValidateRailSet enforces it), so a hardcoded
	// "live" here can never resolve under test_mode.
	env := s.railMerchantAccountEnvironment()
	scope, ok, err := scopeResolver.ActiveRailMerchantAccountScope(ctx, tid, string(models.RailCCBill), env)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("missing scoped merchant CCBill provider account")
	}
	cfg := &config.CCBillConfig{}
	if base != nil {
		*cfg = *base
	}
	// #697: CCBill account_id is dash-joined (clientAccnum-clientSubacc, e.g.
	// 945280-0000). Both parts are numeric, so the first dash is the separator.
	acc, sub, ok := strings.Cut(strings.TrimSpace(scope.AccountID), "-")
	if !ok || strings.TrimSpace(acc) == "" || strings.TrimSpace(sub) == "" {
		return nil, errors.New("CCBill account_id uses a dash: clientAccnum-clientSubacc, e.g. 945280-0000")
	}
	cfg.ClientAccNum = strings.TrimSpace(acc)
	cfg.ClientSubAcc = strings.TrimSpace(sub)

	for _, item := range []struct {
		key string
		dst *string
	}{
		{key: "salt", dst: &cfg.Salt},
		{key: "datalink_username", dst: &cfg.DataLinkUsername},
		{key: "datalink_password", dst: &cfg.DataLinkPassword},
	} {
		value, ok, err := s.merchantProviderSecret(ctx, string(models.RailCCBill), env, item.key)
		if err != nil {
			return nil, fmt.Errorf("load merchant CCBill %s: %w", item.key, err)
		}
		if ok {
			*item.dst = value
		}
	}
	if (strings.TrimSpace(cfg.DataLinkUsername) == "") != (strings.TrimSpace(cfg.DataLinkPassword) == "") {
		return nil, errors.New("merchant CCBill DataLink requires both datalink_username and datalink_password")
	}
	return cfg, nil
}

func (s *CheckoutService) railConfig(name string) *config.RailMerchantAccountConfig {
	if s == nil || s.Rails == nil {
		return nil
	}
	return s.Rails.GetRail(name)
}

// activeNMIConfig returns the configured active NMI provider account, if any.
func (s *CheckoutService) activeNMIConfig() *config.RailMerchantAccountConfig {
	if s == nil || s.Rails == nil {
		return nil
	}
	_, proc, _ := s.Rails.ActiveRailByType(models.RailNMI)
	return proc
}

func cloneRailConfig(in *config.RailMerchantAccountConfig) *config.RailMerchantAccountConfig {
	if in == nil {
		return nil
	}
	out := *in
	if in.Solana != nil && in.Solana.Tokens != nil {
		out.Solana = &config.SolanaRailConfig{}
		*out.Solana = *in.Solana
		out.Solana.Tokens = make(map[string]config.TokenConfig, len(in.Solana.Tokens))
		for k, v := range in.Solana.Tokens {
			out.Solana.Tokens[k] = v
		}
	}
	return &out
}
