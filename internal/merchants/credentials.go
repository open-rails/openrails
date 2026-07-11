package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// DefaultNMICollectJSURL is the standard NMI Collect.js script URL.
const DefaultNMICollectJSURL = "https://secure.networkmerchants.com/token/Collect.js"

// ErrNoActiveRailMerchantAccount means the merchant has provider accounts on the
// requested rail/environment, but all are archived and therefore drain-only.
var ErrNoActiveRailMerchantAccount = errors.New("merchants: no non-archived provider account available for new work")

// StripeCredentials are a merchant's rail credentials, loaded by merchant id at
// request time (NOT injected process-wide). Empty fields mean "not configured".
type StripeCredentials struct {
	SecretKey            string
	WebhookSigningSecret string
	WebhookSigningThin   string
}

// NMITokenizationConfig is browser-facing NMI tokenization configuration for a
// merchant/provider. The key is public, but it is still loaded from the
// merchant-scoped provider config so merchant A's browser config does not bleed
// into merchant B.
type NMITokenizationConfig struct {
	TokenizationKey string
	CollectJSURL    string
}

// LoadStripeCredentials loads a merchant's Stripe credentials from the secret store
// (issue #225). A missing individual secret is not an error — it is returned as an
// empty field — so a merchant that has only a webhook secret (or only an API key)
// still loads. A nil secret store yields empty credentials.
func (s *Service) LoadStripeCredentials(ctx context.Context, id merchant.ID) (StripeCredentials, error) {
	var creds StripeCredentials
	if s.secrets == nil {
		return creds, nil
	}
	if s.pool == nil {
		return s.loadStripeCredentialsByName(ctx, id, SecretStripeSecretKey, SecretStripeWebhookSigning, SecretStripeWebhookSigningThin)
	}
	scope, ok, err := s.activePSPSecretScope(ctx, id, "stripe", s.providerEnvironment)
	if err != nil {
		return creds, err
	}
	if !ok {
		return creds, nil
	}
	secretKeyName, err := scope.secretName("secret_key")
	if err != nil {
		return creds, err
	}
	webhookName, err := scope.secretName("webhook_signing_secret")
	if err != nil {
		return creds, err
	}
	thinName, err := scope.secretName("webhook_signing_secret_thin")
	if err != nil {
		return creds, err
	}
	return s.loadStripeCredentialsByName(ctx, id, secretKeyName, webhookName, thinName)
}

func (s *Service) LoadNMIWebhookSigningSecret(ctx context.Context, id merchant.ID, provider string) (string, error) {
	if s.secrets == nil || id.IsZero() {
		return "", nil
	}
	if strings.ToLower(strings.TrimSpace(provider)) != string(models.RailNMI) {
		return "", nil
	}
	if s.pool == nil {
		return s.secretValue(ctx, id, SecretNMIWebhookSigning)
	}
	scope, ok, err := s.activePSPSecretScope(ctx, id, "nmi", s.providerEnvironment)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	secretName, err := scope.secretName("webhook_signing_secret")
	if err != nil {
		return "", err
	}
	return s.secretValue(ctx, id, secretName)
}

// LoadNMITokenizationConfig loads merchant-scoped browser tokenization config
// for an NMI provider. Missing values return empty fields, except CollectJSURL
// defaults to NMI's standard URL when a supported provider is selected.
func (s *Service) LoadNMITokenizationConfig(ctx context.Context, id merchant.ID, provider string) (NMITokenizationConfig, error) {
	var cfg NMITokenizationConfig
	if id.IsZero() {
		return cfg, nil
	}

	collectURL := ""
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case string(models.RailNMI):
		if s.pool == nil {
			cfg.CollectJSURL = DefaultNMICollectJSURL
			return cfg, nil
		}
		scope, ok, err := s.activePSPSecretScope(ctx, id, "nmi", s.providerEnvironment)
		if err != nil {
			return cfg, err
		}
		if !ok {
			cfg.CollectJSURL = DefaultNMICollectJSURL
			return cfg, nil
		}
		cfg.TokenizationKey = scope.setting("tokenization_key")
		collectURL = scope.setting("tokenization_url")
	default:
		return cfg, nil
	}

	cfg.CollectJSURL = collectURL
	if cfg.CollectJSURL == "" {
		cfg.CollectJSURL = DefaultNMICollectJSURL
	}
	return cfg, nil
}

func (s *Service) loadStripeCredentialsByName(ctx context.Context, id merchant.ID, secretKeyName, webhookName, thinName string) (StripeCredentials, error) {
	var creds StripeCredentials
	var err error
	if creds.SecretKey, err = s.secretValue(ctx, id, secretKeyName); err != nil {
		return creds, err
	}
	if creds.WebhookSigningSecret, err = s.secretValue(ctx, id, webhookName); err != nil {
		return creds, err
	}
	if creds.WebhookSigningThin, err = s.secretValue(ctx, id, thinName); err != nil {
		return creds, err
	}
	return creds, nil
}

func (s *Service) secretValue(ctx context.Context, id merchant.ID, name string) (string, error) {
	sec, err := s.secrets.Get(ctx, id, name)
	if err != nil {
		if errors.Is(err, ErrSecretNotFound) {
			return "", nil
		}
		return "", err
	}
	return sec.Value, nil
}

type pspSecretScope struct {
	id          uuid.UUID
	rail        string
	environment string
	accountID   string
	displayName string
	settings    map[string]any
}

func (s pspSecretScope) secretName(key string) (string, error) {
	return PSPSecretName(s.rail, s.environment, s.accountID, key)
}

func (s pspSecretScope) setting(key string) string {
	if len(s.settings) == 0 {
		return ""
	}
	switch v := s.settings[key].(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func (s pspSecretScope) exported() PSPScope {
	settings := map[string]any(nil)
	if len(s.settings) > 0 {
		settings = make(map[string]any, len(s.settings))
		for k, v := range s.settings {
			settings[k] = v
		}
	}
	return PSPScope{
		ID:          s.id,
		Rail:        s.rail,
		Environment: s.environment,
		AccountID:   s.accountID,
		DisplayName: s.displayName,
		Settings:    settings,
	}
}

// ActivePSPSecretName resolves the active provider account for a
// merchant rail/environment and returns that account's scoped secret
// name for key.
func (s *Service) ActivePSPSecretName(ctx context.Context, id merchant.ID, rail, environment, key string) (string, bool, error) {
	scope, ok, err := s.activePSPSecretScope(ctx, id, rail, environment)
	if err != nil || !ok {
		return "", ok, err
	}
	name, err := scope.secretName(key)
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// ActivePSPScope resolves the active provider account for a merchant
// rail/environment.
func (s *Service) ActivePSPScope(ctx context.Context, id merchant.ID, rail, environment string) (PSPScope, bool, error) {
	scope, ok, err := s.activePSPSecretScope(ctx, id, rail, environment)
	if err != nil || !ok {
		return PSPScope{}, ok, err
	}
	return scope.exported(), true, nil
}

func (s *Service) activePSPSecretScope(ctx context.Context, id merchant.ID, rail, environment string) (pspSecretScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return pspSecretScope{}, false, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return pspSecretScope{}, false, fmt.Errorf("provider account environment must be live or test")
	}
	rail = normalizeProviderSecretType(rail)
	var row gen.OpenrailsPsp
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		count, err := q.CountActivePSPsForNewWork(ctx, gen.CountActivePSPsForNewWorkParams{
			MerchantID:  id.UUID(),
			Rail:        rail,
			Environment: &environment,
		})
		if err != nil {
			return err
		}
		if count > 1 {
			log.WithFields(log.Fields{
				"merchant_id":  id.String(),
				"rail":         rail,
				"environment":  environment,
				"active_count": count,
			}).Warn("multiple active provider accounts configured; using newest for new work")
		}
		if count == 0 {
			total, err := q.CountPSPsForRailEnvironment(ctx, gen.CountPSPsForRailEnvironmentParams{
				MerchantID:  id.UUID(),
				Rail:        rail,
				Environment: &environment,
			})
			if err != nil {
				return err
			}
			if total > 0 {
				return ErrNoActiveRailMerchantAccount
			}
		}
		row, err = q.GetActivePSPForNewWork(ctx, gen.GetActivePSPForNewWorkParams{
			MerchantID:  id.UUID(),
			Rail:        rail,
			Environment: &environment,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pspSecretScope{}, false, nil
	}
	if errors.Is(err, ErrNoActiveRailMerchantAccount) {
		return pspSecretScope{}, false, err
	}
	if err != nil {
		return pspSecretScope{}, false, fmt.Errorf("load active provider account %s/%s: %w", rail, environment, err)
	}
	scope := pspSecretScope{
		id:          row.ID,
		rail:        row.Rail,
		environment: row.Environment,
		accountID:   row.AccountID,
		settings:    pspSettings(row.Evidence),
	}
	if row.Key != nil {
		scope.displayName = strings.TrimSpace(*row.Key)
	}
	return scope, true, nil
}

// PSPScopeByKey resolves a declared, non-archived account by
// its manifest account KEY (display_name, e.g. "mobius") for the given
// environment. This is how the payment-provider vocabulary used by the catalog
// and checkout resolves to a concrete account.
func (s *Service) PSPScopeByKey(ctx context.Context, id merchant.ID, key, environment string) (PSPScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || strings.TrimSpace(key) == "" {
		return PSPScope{}, false, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return PSPScope{}, false, fmt.Errorf("provider account environment must be live or test")
	}
	var scope pspSecretScope
	var evidence []byte
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
				SELECT id, rail, environment, account_id, COALESCE(display_name, ''), evidence
				  FROM openrails.psps
				 WHERE merchant_id = $1::uuid
				   AND lower(display_name) = lower($2)
				   AND environment = $3
				   AND archived = false
				 ORDER BY created_at DESC, id DESC
				 LIMIT 1
			`, id.String(), strings.TrimSpace(key), environment).
			Scan(&scope.id, &scope.rail, &scope.environment, &scope.accountID, &scope.displayName, &evidence)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PSPScope{}, false, nil
	}
	if err != nil {
		return PSPScope{}, false, fmt.Errorf("load provider account by key %s/%s: %w", key, environment, err)
	}
	scope.settings = pspSettings(evidence)
	return scope.exported(), true, nil
}

// pspSecretScopeByAccountID resolves the secret scope for a specific
// account by its account_id (#641). Archived accounts remain addressable for
// inbound webhooks and existing provider-bound obligations.
func (s *Service) pspSecretScopeByAccountID(ctx context.Context, id merchant.ID, rail, accountID string) (pspSecretScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || strings.TrimSpace(accountID) == "" {
		return pspSecretScope{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	var scope pspSecretScope
	var evidence []byte
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
				SELECT rail, environment, account_id, COALESCE(display_name, ''), evidence
				  FROM openrails.psps
				 WHERE merchant_id = $1::uuid
				   AND rail = lower($2)
				   AND account_id = $3
				 LIMIT 1
			`, id.String(), rail, strings.TrimSpace(accountID)).Scan(&scope.rail, &scope.environment, &scope.accountID, &scope.displayName, &evidence)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pspSecretScope{}, false, nil
	}
	if err != nil {
		return pspSecretScope{}, false, fmt.Errorf("load provider account %s/%s: %w", rail, accountID, err)
	}
	scope.settings = pspSettings(evidence)
	return scope, true, nil
}

func pspSettings(raw []byte) map[string]any {
	var evidence struct {
		Settings map[string]any `json:"settings"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &evidence) != nil || len(evidence.Settings) == 0 {
		return nil
	}
	return evidence.Settings
}

// PullRailMerchantAccountScope resolves the provider account the PULL plane
// (provider refresh, unknown-cohort resolution, probes — #699) reads with: the
// active account for new work when one exists, else the NEWEST archived
// account. Archived accounts stay pull-addressable so existing obligations can
// drain (#655) — only NEW checkout/subscription work excludes them. ok=false
// when the merchant declares no account on the rail/environment at all.
func (s *Service) PullRailMerchantAccountScope(ctx context.Context, id merchant.ID, rail, environment string) (PSPScope, bool, error) {
	scope, ok, err := s.activePSPSecretScope(ctx, id, rail, environment)
	if errors.Is(err, ErrNoActiveRailMerchantAccount) {
		return s.newestRailMerchantAccountScope(ctx, id, rail, environment)
	}
	if err != nil || !ok {
		return PSPScope{}, ok, err
	}
	return scope.exported(), true, nil
}

// newestRailMerchantAccountScope returns the newest declared account for
// rail/environment regardless of archived state (the #699 drain-pull leg).
func (s *Service) newestRailMerchantAccountScope(ctx context.Context, id merchant.ID, rail, environment string) (PSPScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return PSPScope{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return PSPScope{}, false, fmt.Errorf("provider account environment must be live or test")
	}
	var scope pspSecretScope
	var evidence []byte
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
				SELECT id, rail, environment, account_id, evidence
				  FROM openrails.psps
				 WHERE merchant_id = $1::uuid
				   AND rail = lower($2)
				   AND environment = $3
				 ORDER BY created_at DESC, id DESC
				 LIMIT 1
			`, id.String(), rail, environment).Scan(&scope.id, &scope.rail, &scope.environment, &scope.accountID, &evidence)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PSPScope{}, false, nil
	}
	if err != nil {
		return PSPScope{}, false, fmt.Errorf("load newest provider account %s/%s: %w", rail, environment, err)
	}
	scope.settings = pspSettings(evidence)
	return scope.exported(), true, nil
}

// PSPScopeByAccountID resolves a specific declared account by
// its rail-native account_id (#641). Archived accounts remain addressable —
// operator pulls may target them for drain.
func (s *Service) PSPScopeByAccountID(ctx context.Context, id merchant.ID, rail, accountID string) (PSPScope, bool, error) {
	scope, ok, err := s.pspSecretScopeByAccountID(ctx, id, rail, accountID)
	if err != nil || !ok {
		return PSPScope{}, ok, err
	}
	return scope.exported(), true, nil
}

// LoadNMIWebhookSigningSecretForAccount loads the webhook secret for a specific NMI
// account (#641). ok=false when no such enabled account — the caller must reject.
func (s *Service) LoadNMIWebhookSigningSecretForAccount(ctx context.Context, id merchant.ID, accountID string) (string, bool, error) {
	if s.secrets == nil || id.IsZero() {
		return "", false, nil
	}
	scope, ok, err := s.pspSecretScopeByAccountID(ctx, id, string(models.RailNMI), accountID)
	if err != nil || !ok {
		return "", ok, err
	}
	secretName, err := scope.secretName("webhook_signing_secret")
	if err != nil {
		return "", false, err
	}
	secret, err := s.secretValue(ctx, id, secretName)
	return secret, true, err
}

// LoadStripeCredentialsForAccount loads credentials for a specific Stripe account
// by account_id (#641). ok=false when no such enabled account.
func (s *Service) LoadStripeCredentialsForAccount(ctx context.Context, id merchant.ID, accountID string) (StripeCredentials, bool, error) {
	var creds StripeCredentials
	if s.secrets == nil {
		return creds, false, nil
	}
	scope, ok, err := s.pspSecretScopeByAccountID(ctx, id, "stripe", accountID)
	if err != nil || !ok {
		return creds, ok, err
	}
	secretKeyName, err := scope.secretName("secret_key")
	if err != nil {
		return creds, false, err
	}
	webhookName, err := scope.secretName("webhook_signing_secret")
	if err != nil {
		return creds, false, err
	}
	thinName, err := scope.secretName("webhook_signing_secret_thin")
	if err != nil {
		return creds, false, err
	}
	c, err := s.loadStripeCredentialsByName(ctx, id, secretKeyName, webhookName, thinName)
	return c, true, err
}

// ResolvePSPID returns the row id of an account by account_id
// (#641), to stamp records from a per-account webhook. ok=false when none matches.
func (s *Service) ResolvePSPID(ctx context.Context, id merchant.ID, rail, accountID string) (uuid.UUID, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || strings.TrimSpace(accountID) == "" {
		return uuid.Nil, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	var pid uuid.UUID
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id FROM openrails.psps
			 WHERE merchant_id = $1::uuid
			   AND rail = lower($2)
			   AND account_id = $3
			 LIMIT 1
		`, id.String(), rail, strings.TrimSpace(accountID)).Scan(&pid)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return pid, true, nil
}

// RailMerchantAccountIdentity is the globally unique rail-native provider
// account identity plus the merchant that owns it.
type RailMerchantAccountIdentity struct {
	ID          uuid.UUID
	MerchantID  merchant.ID
	Rail        string
	Environment string
	AccountID   string
}

// ErrRailMerchantAccountOwnedByAnotherMerchant signals that a (rail, environment,
// account_id) is already registered to a DIFFERENT merchant. Provider accounts
// are globally unique to one merchant (#650) and are never shared or moved; the
// upsert rejects a cross-merchant claim, but with an opaque no-rows /
// unique-violation error — this names the conflict instead.
var ErrRailMerchantAccountOwnedByAnotherMerchant = errors.New("provider account is already owned by another merchant")

// PSPNaturalKey canonicalizes a provider account's GLOBAL
// natural key (rail, environment, account_id) and derives its deterministic id
// from it (#662). It is the SINGLE place this canonicalization lives — the same
// normalization the ownership guard and secret layer use (lower(rail);
// environment mapped to live/test; account_id trimmed). Every writer stores the
// returned nRail/nEnv/nAccount so the stored row matches the id and the
// (rail, environment, account_id) unique index exactly; the id is a pure
// function of that key, identical across environments and fresh rebuilds.
func PSPNaturalKey(rail, environment, accountID string) (id uuid.UUID, nRail, nEnv, nAccount string) {
	nRail = normalizeProviderSecretType(rail)
	nEnv = normalizeProviderSecretEnvironment(environment)
	nAccount = strings.TrimSpace(accountID)
	id = uuidutil.DeterministicID(uuidutil.DeterministicNamespace, nRail, nEnv, nAccount)
	return id, nRail, nEnv, nAccount
}

// PspID returns just the deterministic id for a provider
// account's natural key (#662) — for callers that already hold the row and only
// need to compute or match its id.
func PspID(rail, environment, accountID string) uuid.UUID {
	id, _, _, _ := PSPNaturalKey(rail, environment, accountID)
	return id
}

// AssertPSPUnowned is a clear-error preflight for the
// global-uniqueness upsert: it returns ErrRailMerchantAccountOwnedByAnotherMerchant
// (wrapped with the conflicting identity) when (rail, environment, account_id)
// already belongs to a merchant other than merchantID, and nil when the account
// is unclaimed or already this merchant's. q MUST be a privileged (non-RLS)
// querier so it can see accounts across merchants.
func AssertPSPUnowned(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, rail, environment, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if q == nil || accountID == "" {
		return nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return errors.New("merchants: provider account environment must be live or test")
	}
	row, err := q.GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{
		Rail:        rail,
		Environment: &environment,
		AccountID:   accountID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.MerchantID != merchantID {
		return fmt.Errorf("provider account %s:%s (%s): %w", row.Rail, row.AccountID, row.Environment, ErrRailMerchantAccountOwnedByAnotherMerchant)
	}
	return nil
}

// ResolveRailMerchantAccountByIdentity resolves an account globally by
// its rail-native identity. Use this at webhook/callback boundaries where the
// provider payload or route carries account_id and the merchant should be derived
// from the account row.
func (s *Service) ResolveRailMerchantAccountByIdentity(ctx context.Context, rail, environment, accountID string) (RailMerchantAccountIdentity, bool, error) {
	if s == nil || s.pool == nil || strings.TrimSpace(accountID) == "" {
		return RailMerchantAccountIdentity{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return RailMerchantAccountIdentity{}, false, errors.New("merchants: provider account environment must be live or test")
	}
	row, err := gen.New(s.pool).GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{
		Rail:        rail,
		Environment: &environment,
		AccountID:   strings.TrimSpace(accountID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RailMerchantAccountIdentity{}, false, nil
	}
	if err != nil {
		return RailMerchantAccountIdentity{}, false, err
	}
	return RailMerchantAccountIdentity{
		ID:          row.ID,
		MerchantID:  merchant.ID(row.MerchantID),
		Rail:        row.Rail,
		Environment: row.Environment,
		AccountID:   row.AccountID,
	}, true, nil
}

// HasLiveRailMerchantAccounts reports whether ANY merchant declares a provider
// account on rail with environment=live (global, cross-merchant — same trust
// boundary as ResolveRailMerchantAccountByIdentity). Webhook ingestion uses
// it (#668): CCBill has no HMAC, so its test_mode IP-allowlist bypass is
// refused while a live CCBill account exists anywhere in the catalog.
func (s *Service) HasLiveRailMerchantAccounts(ctx context.Context, rail string) (bool, error) {
	if s == nil || s.pool == nil {
		return false, errors.New("merchants: pgx pool is required")
	}
	rail = normalizeProviderSecretType(rail)
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM openrails.psps
			 WHERE rail = lower($1) AND environment = 'live'
		)`, rail).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("merchants: probe live %s provider accounts: %w", rail, err)
	}
	return exists, nil
}

// PutCredential stores/rotates a single per-merchant credential.
func (s *Service) PutCredential(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	if s.secrets == nil {
		return Secret{}, errors.New("merchants: no secret store configured")
	}
	name = cleanSecretName(name)
	if !SecretWritable(name) {
		return Secret{}, fmt.Errorf("merchants: unknown merchant secret %q", name)
	}
	if err := validateSecretValueLocal(name, value); err != nil {
		return Secret{}, fmt.Errorf("merchants: validate merchant secret %q: %w", name, err)
	}
	sec, err := s.secrets.Put(ctx, id, name, value)
	if err != nil {
		return Secret{}, err
	}
	return sec, nil
}

// RotateCredential is PutCredential with action="rotate".
func (s *Service) RotateCredential(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	return s.PutCredential(ctx, id, name, value)
}

// TestStripeCredential verifies a merchant's stored Stripe secret key works WITHOUT
// charging, by listing the account's balance via the Stripe API. tester is the
// verification function (so this stays testable without a live Stripe); when nil,
// a default real Stripe balance check is used. It records a "test" audit row.
func (s *Service) TestStripeCredential(ctx context.Context, id merchant.ID, tester func(ctx context.Context, secretKey string) error) error {
	return s.ValidateCredential(ctx, id, SecretStripeSecretKey, "", tester)
}
