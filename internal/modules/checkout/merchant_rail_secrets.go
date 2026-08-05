package checkout

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/railresolve"
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

func (s *CheckoutService) SetRailMerchantAccountSecretResolver(resolver merchants.PSPSecretResolver) {
	if s == nil {
		return
	}
	s.ProviderSecrets = resolver
}

func (s *CheckoutService) merchantSecret(ctx context.Context, name string) (string, bool, error) {
	return s.merchantSecretRef(ctx, merchants.SecretRef{Name: name})
}

// merchantSecretRef is the single credential read for checkout money paths. It
// carries the or#812 rotation version floor, so a credential rotated on ANOTHER
// node is picked up on the next charge instead of after a cache TTL of
// presenting a retired key to the gateway.
func (s *CheckoutService) merchantSecretRef(ctx context.Context, ref merchants.SecretRef) (string, bool, error) {
	if s == nil || s.MerchantSecrets == nil {
		return "", false, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", false, err
	}
	sec, err := merchants.ReadSecretRef(ctx, s.MerchantSecrets, tid, ref)
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
	if refResolver, ok := s.ProviderSecrets.(merchants.PSPSecretRefResolver); ok {
		ref, found, err := refResolver.ActivePSPSecretRef(ctx, tid, rail, environment, key)
		if err != nil || !found {
			return "", found, err
		}
		return s.merchantSecretRef(ctx, ref)
	}
	name, ok, err := s.ProviderSecrets.ActivePSPSecretName(ctx, tid, rail, environment, key)
	if err != nil || !ok {
		return "", ok, err
	}
	return s.merchantSecret(ctx, name)
}

func (s *CheckoutService) scopedProviderSecretsEnabled() bool {
	return s != nil && s.MerchantSecrets != nil && s.ProviderSecrets != nil
}

// pspEnvironment is the environment provider-account rows carry in
// this deployment: test under test_mode, live otherwise (#641).
func (s *CheckoutService) pspEnvironment() string {
	return config.ExpectedProviderEnvironment(s != nil && s.Config != nil && s.Config.IsTestMode())
}

// railTarget is a resolved checkout destination: the payment PROVIDER (the
// merchant's account key, e.g. "mobius") plus the rail it runs on. Checkout
// requests name the provider; a bare rail name resolves through arming to THE
// active provider on that rail, so the provider is always picked.
type railTarget struct {
	PSP   string // PSP key: catalog psp_links key, plan lookup key
	Rail  string // gateway rail: dispatch, row vocabulary (subscriptions.rail)
	Scope *merchants.PSPScope
}

// knownRails are the gateway rails checkout can dispatch to.
var knownRails = map[string]struct{}{
	string(models.RailNMI):    {},
	string(models.RailCCBill): {},
	string(models.RailStripe): {},
	string(models.RailSolana): {},
}

// AmbiguousRailError reports a bare rail-kind selector that matched more than
// one armed PSP: the wire must name the PSP key (#848).
type AmbiguousRailError struct {
	Rail string
	Keys []string
}

func (e *AmbiguousRailError) Error() string {
	return fmt.Sprintf("ambiguous rail %q: multiple armed PSPs (%s); pass the PSP key", e.Rail, strings.Join(e.Keys, ", "))
}

// UnknownRailError reports a wire selector that is neither a declared PSP key
// nor a rail kind.
type UnknownRailError struct{ Selector string }

func (e *UnknownRailError) Error() string {
	return fmt.Sprintf("unknown payment provider %q: not a declared PSP key or a rail (nmi, ccbill, stripe, solana)", e.Selector)
}

// resolveRailTarget resolves a requested checkout provider name. The name is
// either a declared PSP key ("mobius"), or a rail kind — reserved gateways
// (stripe/ccbill/solana) are their own PSP names, and a bare rail kind resolves
// to its armed PSP only when that is unambiguous (exactly one armed account).
// Unknown names and ambiguous rail kinds fail loudly.
func (s *CheckoutService) resolveRailTarget(ctx context.Context, requested string) (railTarget, error) {
	name := strings.ToLower(strings.TrimSpace(requested))
	if name == "" {
		return railTarget{}, errors.New("rail is required")
	}
	_, isRail := knownRails[name]

	// Declared PSP key wins (a rail-named account resolves identically).
	if keys, ok := s.ProviderSecrets.(merchants.PSPKeyResolver); ok {
		if tid, err := merchant.Require(ctx); err == nil {
			scope, found, err := keys.PSPScopeByKey(ctx, tid, name, s.pspEnvironment())
			if err != nil {
				return railTarget{}, fmt.Errorf("resolve payment provider %q: %w", name, err)
			}
			if found {
				return railTarget{PSP: name, Rail: scope.Rail, Scope: &scope}, nil
			}
		}
	}

	if !isRail {
		return railTarget{}, &UnknownRailError{Selector: name}
	}

	// Bare rail kind: arming picks the PSP, and only an unambiguous match may.
	// The armed account's key becomes the PSP so plan links resolve; an unwired
	// resolver leaves the rail name (single-account deployments declare links
	// under it).
	target := railTarget{PSP: name, Rail: name}
	adopt := func(scope merchants.PSPScope) {
		target.Scope = &scope
		if key := strings.ToLower(strings.TrimSpace(scope.Key)); key != "" {
			target.PSP = key
		}
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return target, nil
	}
	if lister, ok := s.ProviderSecrets.(merchants.PSPRailScopesResolver); ok {
		scopes, err := lister.ActivePSPScopesForRail(ctx, tid, name, s.pspEnvironment())
		switch {
		case err != nil:
			log.WithContext(ctx).WithError(err).WithField("rail", strconv.Quote(name)).Debug("checkout: provider-account resolution failed; proceeding rail-scoped")
		case len(scopes) == 1:
			adopt(scopes[0])
		case len(scopes) > 1:
			keys := make([]string, 0, len(scopes))
			for _, scope := range scopes {
				key := strings.ToLower(strings.TrimSpace(scope.Key))
				if key == "" {
					key = scope.AccountID
				}
				keys = append(keys, key)
			}
			return railTarget{}, &AmbiguousRailError{Rail: name, Keys: keys}
		}
		return target, nil
	}
	// Legacy resolvers without the list capability: single-account semantics.
	if scopes, ok := s.ProviderSecrets.(merchants.PSPScopeResolver); ok {
		scope, found, err := scopes.ActivePSPScope(ctx, tid, name, s.pspEnvironment())
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("rail", strconv.Quote(name)).Debug("checkout: provider-account resolution failed; proceeding rail-scoped")
		} else if found {
			adopt(scope)
		}
	}
	return target, nil
}

// CheckoutRailUsable reports whether a wire payment.rail selector (a PSP key,
// or an unambiguous rail kind) lands on an armed PSP for the ctx merchant.
// nil means usable; the error carries the reject reason. Fail closed.
func (s *CheckoutService) CheckoutRailUsable(ctx context.Context, selector string) error {
	if s == nil {
		return errors.New("unsupported rail")
	}
	target, err := s.resolveRailTarget(ctx, selector)
	if err != nil {
		return err
	}
	if target.Scope == nil {
		return errors.New("unsupported rail")
	}
	return nil
}

// ResolvePSPID resolves the PSP for new work on the given provider/rail name
// for provenance stamping. Returns uuid.Nil when no resolver is wired or
// nothing is armed — provenance is only ever stamped with a REAL resolved
// account, never invented. or#893: the CALLER decides what an unresolved PSP
// means; every provider-bound write now refuses one.
func (s *CheckoutService) ResolvePSPID(ctx context.Context, name string) uuid.UUID {
	if s == nil || s.ProviderSecrets == nil {
		return uuid.Nil
	}
	target, err := s.resolveRailTarget(ctx, name)
	if err != nil || target.Scope == nil {
		return uuid.Nil
	}
	return target.Scope.ID
}

// railSource exposes the merchant rail plane for routing (checkoutRailTargets).
func (s *CheckoutService) railSource() railresolve.Source {
	if s == nil {
		return nil
	}
	return s.Rails
}

// stampPSP pins resolved account provenance into ctx so the
// payment / subscription / payment-method writes downstream of this checkout
// flow stamp psp_id (#704).
func (s *CheckoutService) stampPSP(ctx context.Context, name string) context.Context {
	return db.WithPSPID(ctx, s.ResolvePSPID(ctx, name))
}

// resolveNMIClient arms the ctx merchant's NMI client for the given provider
// name (account key or bare rail): the resolved account's security_key. Fail
// closed — a missing scope/secret errors; there is no boot-config fallback.
func (s *CheckoutService) resolveNMIClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	if s.ResolveNMIClientOverride != nil {
		return s.ResolveNMIClientOverride(ctx, provider)
	}
	if !s.scopedProviderSecretsEnabled() {
		return nil, fmt.Errorf("merchant rail resolution is not configured")
	}
	target, err := s.resolveRailTarget(ctx, provider)
	if err != nil {
		return nil, err
	}
	if !rails.IsNMI(models.Rail(target.Rail)) {
		return nil, fmt.Errorf("missing client")
	}
	var value string
	if target.Scope != nil {
		// Pinned to the resolved account's own secret, at or above the rotation
		// version floor that account's PSP row records (or#812).
		ref, err := target.Scope.SecretRef("security_key")
		if err != nil {
			return nil, err
		}
		v, ok, err := s.merchantSecretRef(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("load merchant NMI secret: %w", err)
		}
		if !ok {
			return nil, fmt.Errorf("missing scoped merchant NMI secret for provider account")
		}
		value = v
	} else {
		// Environment follows test_mode, same as the CCBill leg.
		v, ok, err := s.merchantProviderSecret(ctx, string(models.RailNMI), s.pspEnvironment(), "security_key")
		if err != nil {
			return nil, fmt.Errorf("load merchant NMI secret: %w", err)
		}
		if !ok {
			return nil, fmt.Errorf("missing scoped merchant NMI secret for provider account")
		}
		value = v
	}
	proc := &config.PSPConfig{Rail: models.RailNMI, NMI: &config.NMIRailConfig{SecurityKey: value}}
	client, err := nmi.NewClient(target.PSP, proc.ToNMIProviderSettings(), s.Config != nil && s.Config.IsTestMode())
	if err != nil {
		return nil, err
	}
	if s.NMIEndpointOverride != "" {
		client.DirectPostURL = s.NMIEndpointOverride
		client.QueryURL = s.NMIEndpointOverride
		client.V5BaseURL = s.NMIEndpointOverride
	}
	return client, nil
}

func (s *CheckoutService) resolveCCBillClient(ctx context.Context) (*ccbill.CCBillClient, error) {
	cfg, err := s.resolveCCBillConfig(ctx)
	if err != nil {
		return nil, err
	}
	return ccbill.NewClient(cfg, s.Config != nil && s.Config.IsTestMode()), nil
}

func (s *CheckoutService) resolveCCBillConfig(ctx context.Context) (*config.CCBillConfig, error) {
	if !s.scopedProviderSecretsEnabled() {
		return nil, errors.New("merchant rail resolution is not configured")
	}
	return s.resolveScopedCCBillConfig(ctx, &config.CCBillConfig{})
}

func (s *CheckoutService) resolveScopedCCBillConfig(ctx context.Context, base *config.CCBillConfig) (*config.CCBillConfig, error) {
	scopeResolver, ok := s.ProviderSecrets.(merchants.PSPScopeResolver)
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
	env := s.pspEnvironment()
	scope, ok, err := scopeResolver.ActivePSPScope(ctx, tid, string(models.RailCCBill), env)
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

// custodianHeld reports whether the resolved PSP's instruments are held by a
// third-party custodian (or#879/or#880) — the axis that decides which charge
// transport a checkout takes, orthogonal to the rail that charges. It is a
// plain reference check now: the PSP row either points at a custodians row or
// it does not, so there is no parse here to be ambiguous about.
func custodianHeld(target railTarget) bool {
	return target.Scope != nil && target.Scope.CustodianID != nil
}

// pspKeyArchived reports whether selector names a declared-but-archived PSP
// (or#288). Best-effort by design: it only refines a skip CLASS in the routing
// trace, never a routing outcome, so a resolver that cannot answer leaves the
// class as-is rather than failing the checkout.
func (s *CheckoutService) pspKeyArchived(ctx context.Context, selector string) bool {
	resolver, ok := s.ProviderSecrets.(merchants.ArchivedPSPKeyResolver)
	if !ok {
		return false
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false
	}
	archived, err := resolver.PSPKeyArchived(ctx, tid, selector, s.pspEnvironment())
	if err != nil {
		log.WithContext(ctx).WithError(err).Debug("checkout routing: archived-PSP lookup failed")
		return false
	}
	return archived
}
