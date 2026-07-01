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
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// DefaultNMICollectJSURL is the standard NMI Collect.js script URL.
const DefaultNMICollectJSURL = "https://secure.networkmerchants.com/token/Collect.js"

// ErrNoActiveProviderAccount means the merchant has provider accounts on the
// requested rail/environment, but all are archived and therefore drain-only.
var ErrNoActiveProviderAccount = errors.New("merchants: no non-archived provider account available for new work")

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
	scope, ok, err := s.activeProviderAccountSecretScope(ctx, id, "stripe", "live")
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
	scope, ok, err := s.activeProviderAccountSecretScope(ctx, id, "nmi", "live")
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
		scope, ok, err := s.activeProviderAccountSecretScope(ctx, id, "nmi", "live")
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

type providerAccountSecretScope struct {
	id          uuid.UUID
	rail        string
	environment string
	accountID   string
	settings    map[string]any
}

func (s providerAccountSecretScope) secretName(key string) (string, error) {
	return ProviderAccountSecretName(s.rail, s.environment, s.accountID, key)
}

func (s providerAccountSecretScope) setting(key string) string {
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

func (s providerAccountSecretScope) exported() ProviderAccountScope {
	settings := map[string]any(nil)
	if len(s.settings) > 0 {
		settings = make(map[string]any, len(s.settings))
		for k, v := range s.settings {
			settings[k] = v
		}
	}
	return ProviderAccountScope{
		ID:          s.id,
		Rail:        s.rail,
		Environment: s.environment,
		AccountID:   s.accountID,
		Settings:    settings,
	}
}

// ActiveProviderAccountSecretName resolves the active provider account for a
// merchant rail/environment and returns that account's scoped secret
// name for key.
func (s *Service) ActiveProviderAccountSecretName(ctx context.Context, id merchant.ID, rail, environment, key string) (string, bool, error) {
	scope, ok, err := s.activeProviderAccountSecretScope(ctx, id, rail, environment)
	if err != nil || !ok {
		return "", ok, err
	}
	name, err := scope.secretName(key)
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// ActiveProviderAccountScope resolves the active provider account for a merchant
// rail/environment.
func (s *Service) ActiveProviderAccountScope(ctx context.Context, id merchant.ID, rail, environment string) (ProviderAccountScope, bool, error) {
	scope, ok, err := s.activeProviderAccountSecretScope(ctx, id, rail, environment)
	if err != nil || !ok {
		return ProviderAccountScope{}, ok, err
	}
	return scope.exported(), true, nil
}

func (s *Service) activeProviderAccountSecretScope(ctx context.Context, id merchant.ID, rail, environment string) (providerAccountSecretScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return providerAccountSecretScope{}, false, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return providerAccountSecretScope{}, false, fmt.Errorf("provider account environment must be live or test")
	}
	rail = normalizeProviderSecretType(rail)
	var row gen.OpenrailsPaymentProviderAccount
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		count, err := q.CountActiveProviderAccountsForNewWork(ctx, gen.CountActiveProviderAccountsForNewWorkParams{
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
			total, err := q.CountProviderAccountsForRailEnvironment(ctx, gen.CountProviderAccountsForRailEnvironmentParams{
				MerchantID:  id.UUID(),
				Rail:        rail,
				Environment: &environment,
			})
			if err != nil {
				return err
			}
			if total > 0 {
				return ErrNoActiveProviderAccount
			}
		}
		row, err = q.GetActiveProviderAccountForNewWork(ctx, gen.GetActiveProviderAccountForNewWorkParams{
			MerchantID:  id.UUID(),
			Rail:        rail,
			Environment: &environment,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return providerAccountSecretScope{}, false, nil
	}
	if errors.Is(err, ErrNoActiveProviderAccount) {
		return providerAccountSecretScope{}, false, err
	}
	if err != nil {
		return providerAccountSecretScope{}, false, fmt.Errorf("load active provider account %s/%s: %w", rail, environment, err)
	}
	scope := providerAccountSecretScope{
		id:          row.ID,
		rail:        row.Rail,
		environment: row.Environment,
		accountID:   row.AccountID,
		settings:    providerAccountSettings(row.Evidence),
	}
	return scope, true, nil
}

// providerAccountSecretScopeByAccountID resolves the secret scope for a specific
// account by its account_id (#641). Archived accounts remain addressable for
// inbound webhooks and existing provider-bound obligations.
func (s *Service) providerAccountSecretScopeByAccountID(ctx context.Context, id merchant.ID, rail, accountID string) (providerAccountSecretScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || strings.TrimSpace(accountID) == "" {
		return providerAccountSecretScope{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	var scope providerAccountSecretScope
	var evidence []byte
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
				SELECT rail, environment, account_id, evidence
				  FROM openrails.payment_provider_accounts
				 WHERE merchant_id = $1::uuid
				   AND rail = lower($2)
				   AND account_id = $3
				 LIMIT 1
			`, id.String(), rail, strings.TrimSpace(accountID)).Scan(&scope.rail, &scope.environment, &scope.accountID, &evidence)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return providerAccountSecretScope{}, false, nil
	}
	if err != nil {
		return providerAccountSecretScope{}, false, fmt.Errorf("load provider account %s/%s: %w", rail, accountID, err)
	}
	scope.settings = providerAccountSettings(evidence)
	return scope, true, nil
}

func providerAccountSettings(raw []byte) map[string]any {
	var evidence struct {
		Settings map[string]any `json:"settings"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &evidence) != nil || len(evidence.Settings) == 0 {
		return nil
	}
	return evidence.Settings
}

// LoadNMIWebhookSigningSecretForAccount loads the webhook secret for a specific NMI
// account (#641). ok=false when no such enabled account — the caller must reject.
func (s *Service) LoadNMIWebhookSigningSecretForAccount(ctx context.Context, id merchant.ID, accountID string) (string, bool, error) {
	if s.secrets == nil || id.IsZero() {
		return "", false, nil
	}
	scope, ok, err := s.providerAccountSecretScopeByAccountID(ctx, id, string(models.RailNMI), accountID)
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
	scope, ok, err := s.providerAccountSecretScopeByAccountID(ctx, id, "stripe", accountID)
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

// ResolveProviderAccountID returns the row id of an account by account_id
// (#641), to stamp records from a per-account webhook. ok=false when none matches.
func (s *Service) ResolveProviderAccountID(ctx context.Context, id merchant.ID, rail, accountID string) (uuid.UUID, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() || strings.TrimSpace(accountID) == "" {
		return uuid.Nil, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	var pid uuid.UUID
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id FROM openrails.payment_provider_accounts
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

// PaymentProviderAccountIdentity is the globally unique rail-native provider
// account identity plus the merchant that owns it.
type PaymentProviderAccountIdentity struct {
	ID          uuid.UUID
	MerchantID  merchant.ID
	Rail        string
	Environment string
	AccountID   string
}

// ErrProviderAccountOwnedByAnotherMerchant signals that a (rail, environment,
// account_id) is already registered to a DIFFERENT merchant. Provider accounts
// are globally unique to one merchant (#650) and are never shared or moved; the
// upsert rejects a cross-merchant claim, but with an opaque no-rows /
// unique-violation error — this names the conflict instead.
var ErrProviderAccountOwnedByAnotherMerchant = errors.New("provider account is already owned by another merchant")

// AssertProviderAccountUnowned is a clear-error preflight for the
// global-uniqueness upsert: it returns ErrProviderAccountOwnedByAnotherMerchant
// (wrapped with the conflicting identity) when (rail, environment, account_id)
// already belongs to a merchant other than merchantID, and nil when the account
// is unclaimed or already this merchant's. q MUST be a privileged (non-RLS)
// querier so it can see accounts across merchants.
func AssertProviderAccountUnowned(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, rail, environment, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if q == nil || accountID == "" {
		return nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		environment = "live"
	}
	row, err := q.GetProviderAccountByRailIdentity(ctx, gen.GetProviderAccountByRailIdentityParams{
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
		return fmt.Errorf("provider account %s:%s (%s): %w", row.Rail, row.AccountID, row.Environment, ErrProviderAccountOwnedByAnotherMerchant)
	}
	return nil
}

// ResolvePaymentProviderAccountByIdentity resolves an account globally by
// its rail-native identity. Use this at webhook/callback boundaries where the
// provider payload or route carries account_id and the merchant should be derived
// from the account row.
func (s *Service) ResolvePaymentProviderAccountByIdentity(ctx context.Context, rail, environment, accountID string) (PaymentProviderAccountIdentity, bool, error) {
	if s == nil || s.pool == nil || strings.TrimSpace(accountID) == "" {
		return PaymentProviderAccountIdentity{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		environment = "live"
	}
	row, err := gen.New(s.pool).GetProviderAccountByRailIdentity(ctx, gen.GetProviderAccountByRailIdentityParams{
		Rail:        rail,
		Environment: &environment,
		AccountID:   strings.TrimSpace(accountID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentProviderAccountIdentity{}, false, nil
	}
	if err != nil {
		return PaymentProviderAccountIdentity{}, false, err
	}
	return PaymentProviderAccountIdentity{
		ID:          row.ID,
		MerchantID:  merchant.ID(row.MerchantID),
		Rail:        row.Rail,
		Environment: row.Environment,
		AccountID:   row.AccountID,
	}, true, nil
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
