// Package railresolve is the ONE Layer-C rail resolution seam (#788).
//
// Every decision-time question — "is rail X armed for this merchant?", "give
// me its credentials", "build me a rail client" — resolves an active
// psps row (Layer B) plus scoped secrets through the
// merchant secret-store interface. How the armed state got there (MODE 1
// manifest convergence or MODE 2 API writes — Layer A) is invisible here:
// both converge the same rows and the same secret names, so a new ingestion
// path can never create a new resolution path.
//
// Consumers hold a Source. Production wires Merchants (backed by
// merchants.Service); unit tests may wire FixedSet, a static in-memory stand-in
// for Layer B.
package railresolve

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrRailNotArmed reports that the ctx merchant has no active armed account on
// the requested rail (or a required scoped secret is missing). Consumers MUST
// fail closed on it — reject the webhook/checkout, never default-allow.
var ErrRailNotArmed = errors.New("rail is not armed for this merchant")

// Source resolves per-merchant armed rail state at decision time. The merchant
// is always taken from ctx (merchant.Require) — resolution without a merchant
// fails closed.
type Source interface {
	// Armed reports whether rail has an active armed account for the ctx
	// merchant. Backend errors surface as err (fail closed at the caller).
	Armed(ctx context.Context, rail string) (bool, error)
	// RailConfig resolves the armed account for rail into the typed
	// credential shape the rail integrations consume. accountID "" resolves
	// the ACTIVE account for new work; non-empty pins a declared account
	// (inbound webhook routing #641 — may address archived accounts).
	// Returns ErrRailNotArmed (wrapped) when nothing is armed.
	RailConfig(ctx context.Context, rail string, accountID string) (*config.PSPConfig, error)
}

// MerchantsSource is the production Source: scope rows via merchants.Service
// (psps) + scoped secrets via the merchant secret store.
type MerchantsSource struct {
	Config *config.Config
	// MerchantsFn returns the armed merchants service. Late-bound: manifest
	// hosts arm it at UpsertMerchantConfig time, after the graph is built.
	// nil / returning nil resolves nothing (fail closed).
	MerchantsFn func() *merchants.Service
}

// NewMerchantsSource builds the production resolver over a late-bound
// merchants service.
func NewMerchantsSource(cfg *config.Config, merchantsFn func() *merchants.Service) *MerchantsSource {
	return &MerchantsSource{Config: cfg, MerchantsFn: merchantsFn}
}

func (s *MerchantsSource) service() *merchants.Service {
	if s == nil || s.MerchantsFn == nil {
		return nil
	}
	return s.MerchantsFn()
}

// environment is the deployment's provider-account environment: test under
// test_mode, live otherwise (#681).
func (s *MerchantsSource) environment() string {
	return config.ExpectedProviderEnvironment(s.Config != nil && s.Config.IsTestMode())
}

func (s *MerchantsSource) testMode() bool {
	return s.Config != nil && s.Config.IsTestMode()
}

func (s *MerchantsSource) Armed(ctx context.Context, rail string) (bool, error) {
	_, _, ok, err := s.scope(ctx, rail, "")
	if err != nil {
		if errors.Is(err, ErrRailNotArmed) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// scope resolves the merchant + account row. ok=false (nil err) means "no
// account declared" — Armed treats that as unarmed; RailConfig fails closed.
func (s *MerchantsSource) scope(ctx context.Context, rail, accountID string) (merchant.ID, merchants.PSPScope, bool, error) {
	rail = strings.ToLower(strings.TrimSpace(rail))
	if rail == "" {
		return merchant.ID{}, merchants.PSPScope{}, false, fmt.Errorf("rail is required")
	}
	svc := s.service()
	if svc == nil {
		return merchant.ID{}, merchants.PSPScope{}, false, fmt.Errorf("resolve rail %s: merchants service is not armed: %w", rail, ErrRailNotArmed)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return merchant.ID{}, merchants.PSPScope{}, false, fmt.Errorf("resolve rail %s: %w", rail, err)
	}
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		scope, ok, err := svc.PSPScopeByAccountID(ctx, mid, rail, accountID)
		return mid, scope, ok, err
	}
	scope, ok, err := svc.ActivePSPScope(ctx, mid, rail, s.environment())
	if errors.Is(err, merchants.ErrNoActiveRailMerchantAccount) {
		return mid, merchants.PSPScope{}, false, nil
	}
	return mid, scope, ok, err
}

// secret loads one scoped secret. found=false with nil err means genuinely
// absent; backend errors surface as err.
func (s *MerchantsSource) secret(ctx context.Context, mid merchant.ID, scope merchants.PSPScope, key string) (string, bool, error) {
	svc := s.service()
	if svc == nil || svc.Secrets() == nil {
		return "", false, nil
	}
	name, err := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
	if err != nil {
		return "", false, err
	}
	sec, err := svc.Secrets().Get(ctx, mid, name)
	if errors.Is(err, merchants.ErrSecretNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	value := strings.TrimSpace(sec.Value)
	if value == "" {
		return "", false, nil
	}
	return value, true, nil
}

func (s *MerchantsSource) requireSecret(ctx context.Context, mid merchant.ID, scope merchants.PSPScope, key string) (string, error) {
	value, found, err := s.secret(ctx, mid, scope, key)
	if err != nil {
		return "", fmt.Errorf("load %s secret %s: %w", scope.Rail, key, err)
	}
	if !found {
		return "", fmt.Errorf("%s account %s: required secret %s is missing: %w", scope.Rail, scope.AccountID, key, ErrRailNotArmed)
	}
	return value, nil
}

func (s *MerchantsSource) RailConfig(ctx context.Context, rail, accountID string) (*config.PSPConfig, error) {
	mid, scope, ok, err := s.scope(ctx, rail, accountID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("rail %s account %q: %w", rail, accountID, ErrRailNotArmed)
	}
	out := &config.PSPConfig{
		Key:       scope.Key,
		Rail:      models.Rail(scope.Rail),
		AccountID: scope.AccountID,
	}
	switch models.Rail(scope.Rail) {
	case models.RailStripe:
		secretKey, err := s.requireSecret(ctx, mid, scope, "secret_key")
		if err != nil {
			return nil, err
		}
		signing, _, err := s.secret(ctx, mid, scope, "webhook_signing_secret")
		if err != nil {
			return nil, err
		}
		thin, _, err := s.secret(ctx, mid, scope, "webhook_signing_secret_thin")
		if err != nil {
			return nil, err
		}
		out.Stripe = &config.StripeRailConfig{
			SecretKey:                secretKey,
			WebhookSigningSecret:     signing,
			WebhookSigningSecretThin: thin,
		}
	case models.RailCCBill:
		// Identity is the dash-joined account_id (#697); validated by
		// ToCCBillConfig's SplitCCBillAccountID at client-build time.
		salt, _, err := s.secret(ctx, mid, scope, "salt")
		if err != nil {
			return nil, err
		}
		dlUser, _, err := s.secret(ctx, mid, scope, "datalink_username")
		if err != nil {
			return nil, err
		}
		dlPass, _, err := s.secret(ctx, mid, scope, "datalink_password")
		if err != nil {
			return nil, err
		}
		if (dlUser == "") != (dlPass == "") {
			return nil, fmt.Errorf("ccbill account %s: datalink_username and datalink_password must be set together", scope.AccountID)
		}
		out.CCBill = &config.CCBillRailConfig{
			Salt:             salt,
			DataLinkUsername: dlUser,
			DataLinkPassword: dlPass,
		}
	case models.RailNMI:
		securityKey, err := s.requireSecret(ctx, mid, scope, "security_key")
		if err != nil {
			return nil, err
		}
		signing, _, err := s.secret(ctx, mid, scope, "webhook_signing_secret")
		if err != nil {
			return nil, err
		}
		out.NMI = &config.NMIRailConfig{
			SecurityKey:          securityKey,
			WebhookSigningSecret: signing,
		}
	case models.RailSolana:
		settings, err := config.ParseSolanaAccountSettings(scope.Settings)
		if err != nil {
			return nil, err
		}
		solanaCfg, err := SolanaRailConfigFromSettings(settings, s.testMode())
		if err != nil {
			return nil, fmt.Errorf("solana account %s: %w", scope.AccountID, err)
		}
		out.Solana = solanaCfg
	default:
		return nil, fmt.Errorf("rail %s has no typed credential shape", scope.Rail)
	}
	// Custody rides on TOP of the rail block (or#879): the gateway credentials
	// above charge the card, these say who holds it and how to detokenize it.
	custody, err := s.resolveCustody(ctx, mid, scope)
	if err != nil {
		return nil, err
	}
	out.Custody = custody
	return out, nil
}

// resolveCustody arms a PSP's third-party custody arrangement. nil = the PSP
// holds its own instruments (the overwhelmingly common case). A DECLARED
// custodian with no private key is a fail-closed error, never a silent
// downgrade to "no custody".
func (s *MerchantsSource) resolveCustody(ctx context.Context, mid merchant.ID, scope merchants.PSPScope) (*config.CustodyConfig, error) {
	settings, err := config.ParseCustodySettings(scope.Settings)
	if err != nil {
		return nil, fmt.Errorf("psp %s/%s: %w", scope.Rail, scope.AccountID, err)
	}
	if !settings.ThirdParty() {
		return nil, nil
	}
	apiKey, err := s.requireSecret(ctx, mid, scope, config.CustodianSecretAPIKey)
	if err != nil {
		return nil, fmt.Errorf("psp %s/%s custodian %s: %w", scope.Rail, scope.AccountID, settings.Custodian, err)
	}
	return &config.CustodyConfig{
		Custodian:     settings.Custodian,
		AccountID:     settings.AccountID,
		APIKey:        apiKey,
		PublicAPIKey:  settings.PublicAPIKey,
		NetworkTokens: settings.NetworkTokens,
	}, nil
}

// SolanaRailConfigFromSettings materializes the runtime Solana config from a
// rail account's declared settings: network derives from test_mode alone
// (#349), the token set is exactly what the merchant declared — USDC alone when
// they declared nothing (or#881 select-and-restrict; a built-in symbol is
// SELECTED and its mint is never restated) — and the #360 pricing policy then
// drops tokens that cannot function (degrade-not-die). A misdeclared token set
// is fail-closed: the PSP does not arm rather than arming against a mint nobody
// vouched for.
func SolanaRailConfigFromSettings(settings config.SolanaAccountSettings, testMode bool) (*config.SolanaRailConfig, error) {
	network := "mainnet"
	if testMode {
		network = "devnet"
	}
	resolved, err := solanatokens.ResolveDeclared(network, settings.Tokens)
	if err != nil {
		return nil, err
	}
	out := settings.ApplyTo(&config.SolanaRailConfig{Network: network})
	out.Tokens = solanatokens.NormalizeForNetwork(network, resolved)
	return out, nil
}

// FixedSet is a static Source over an in-memory PSPSet — a
// test stand-in for Layer B. Production graphs must never construct one.
type FixedSet config.PSPSet

func (f FixedSet) Armed(_ context.Context, rail string) (bool, error) {
	railType := models.Rail(strings.ToLower(strings.TrimSpace(rail)))
	_, proc, err := config.PSPSet(f).ActiveRailByType(railType)
	if err != nil {
		return false, err
	}
	return proc != nil, nil
}

func (f FixedSet) RailConfig(_ context.Context, rail, accountID string) (*config.PSPConfig, error) {
	set := config.PSPSet(f)
	railType := models.Rail(strings.ToLower(strings.TrimSpace(rail)))
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		for _, key := range set.RailKeysByType(railType) {
			if set[key].EffectiveAccountID() == accountID {
				return withPSPKey(set[key], key), nil
			}
		}
		return nil, fmt.Errorf("rail %s account %q: %w", rail, accountID, ErrRailNotArmed)
	}
	key, proc, err := set.ActiveRailByType(railType)
	if err != nil {
		return nil, err
	}
	if proc == nil {
		return nil, fmt.Errorf("rail %s: %w", rail, ErrRailNotArmed)
	}
	return withPSPKey(proc, key), nil
}

// withPSPKey returns a copy of proc carrying its resolved PSP key (the set's
// map name), matching MerchantsSource's scope.Key population.
func withPSPKey(proc *config.PSPConfig, key string) *config.PSPConfig {
	out := *proc
	out.Key = strings.ToLower(strings.TrimSpace(key))
	return &out
}
