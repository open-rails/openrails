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
	// WebhookSigningPrevious is the outgoing secret during an api_version
	// rollover (#856). Both endpoints deliver through the overlap, so inbound
	// verification must accept both secrets.
	WebhookSigningPrevious string
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
		return s.loadStripeCredentialsByName(ctx, id, SecretStripeSecretKey, SecretStripeWebhookSigning, SecretStripeWebhookSigningThin, SecretStripeWebhookSigningPrevious)
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
	previousName, err := scope.secretName("webhook_signing_secret_previous")
	if err != nil {
		return creds, err
	}
	return s.loadStripeCredentialsByName(ctx, id, secretKeyName, webhookName, thinName, previousName)
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

func (s *Service) loadStripeCredentialsByName(ctx context.Context, id merchant.ID, secretKeyName, webhookName, thinName, previousName string) (StripeCredentials, error) {
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
	if previousName != "" {
		if creds.WebhookSigningPrevious, err = s.secretValue(ctx, id, previousName); err != nil {
			return creds, err
		}
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
	key         string
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
		Key:         s.key,
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
		scope.key = strings.TrimSpace(*row.Key)
	}
	return scope, true, nil
}

// PSPScopeByKey resolves a declared, non-archived account by
// its manifest account key (e.g. "mobius") for the given
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
				SELECT id, rail, environment, account_id, COALESCE(key, ''), evidence
				  FROM openrails.psps
				 WHERE merchant_id = $1::uuid
				   AND lower(key) = lower($2)
				   AND environment = $3
				   AND archived = false
				 ORDER BY created_at DESC, id DESC
				 LIMIT 1
			`, id.String(), strings.TrimSpace(key), environment).
			Scan(&scope.id, &scope.rail, &scope.environment, &scope.accountID, &scope.key, &evidence)
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

// ActivePSPScopesForRail lists every non-archived provider account declared on
// rail/environment, newest first. Checkout uses it to accept a bare rail-kind
// selector only when it is unambiguous (#848).
func (s *Service) ActivePSPScopesForRail(ctx context.Context, id merchant.ID, rail, environment string) ([]PSPScope, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return nil, nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return nil, fmt.Errorf("provider account environment must be live or test")
	}
	var out []PSPScope
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
				SELECT id, rail, environment, account_id, COALESCE(key, ''), evidence
				  FROM openrails.psps
				 WHERE merchant_id = $1::uuid
				   AND rail = lower($2)
				   AND environment = $3
				   AND archived = false
				 ORDER BY created_at DESC, id DESC
			`, id.String(), rail, environment)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = out[:0]
		for rows.Next() {
			var scope pspSecretScope
			var evidence []byte
			if err := rows.Scan(&scope.id, &scope.rail, &scope.environment, &scope.accountID, &scope.key, &evidence); err != nil {
				return err
			}
			scope.settings = pspSettings(evidence)
			out = append(out, scope.exported())
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list provider accounts %s/%s: %w", rail, environment, err)
	}
	return out, nil
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
				SELECT rail, environment, account_id, COALESCE(key, ''), evidence
				  FROM openrails.psps
				 WHERE merchant_id = $1::uuid
				   AND rail = lower($2)
				   AND account_id = $3
				 LIMIT 1
			`, id.String(), rail, strings.TrimSpace(accountID)).Scan(&scope.rail, &scope.environment, &scope.accountID, &scope.key, &evidence)
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
	previousName, err := scope.secretName("webhook_signing_secret_previous")
	if err != nil {
		return creds, false, err
	}
	c, err := s.loadStripeCredentialsByName(ctx, id, secretKeyName, webhookName, thinName, previousName)
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
// is unclaimed or already this merchant's.
//
// #824: the ownership question is global by definition, so it goes through the
// cross-merchant directory function (migration 0016). Reading psps directly on
// q could only ever see the caller's OWN merchant, which made this assertion
// pass unconditionally — the only thing still catching a hijack was UpsertPSP's
// `ON CONFLICT … WHERE psps.merchant_id = EXCLUDED.merchant_id`, and it reports
// the conflict as an opaque no-rows.
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
	owner, found, err := resolvePSPOwner(ctx, q, rail, environment, accountID)
	if err != nil || !found {
		return err
	}
	if uuid.UUID(owner.MerchantID) != merchantID {
		return fmt.Errorf("provider account %s:%s (%s): %w", owner.Rail, owner.AccountID, owner.Environment, ErrRailMerchantAccountOwnedByAnotherMerchant)
	}
	return nil
}

// ResolveRailMerchantAccountByIdentity resolves an account globally by
// its rail-native identity. Use this at webhook/callback boundaries where the
// provider payload or route carries account_id and the merchant should be derived
// from the account row.
//
// #824: this is a genuinely cross-merchant read — inbound webhooks have no
// merchant context yet, which is the whole point. It used to run
// GetPSPByRailIdentity on the base pool under a comment claiming a "privileged,
// non-RLS role"; no such role exists (one pool, one DSN), so under the
// production openrails_app role psps' FORCE'd merchant_isolation policy made it
// return zero rows and no error, and EVERY account-routed CCBill/Basis
// Theory/Stripe-account webhook answered 404 "Unknown provider account". It now
// goes through the explicit SECURITY DEFINER directory function (migration
// 0016), which raises instead of returning empty when it cannot see across
// merchants.
func (s *Service) ResolveRailMerchantAccountByIdentity(ctx context.Context, rail, environment, accountID string) (RailMerchantAccountIdentity, bool, error) {
	if s == nil || s.pool == nil || strings.TrimSpace(accountID) == "" {
		return RailMerchantAccountIdentity{}, false, nil
	}
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return RailMerchantAccountIdentity{}, false, errors.New("merchants: provider account environment must be live or test")
	}
	return resolvePSPOwner(ctx, gen.New(s.pool), rail, environment, strings.TrimSpace(accountID))
}

// resolvePSPOwner is the one place the cross-merchant PSP directory lookup is
// made. q may be bound to anything (pool, merchant-pinned conn, tx): the
// definer function is what supplies cross-merchant visibility, not the handle.
func resolvePSPOwner(ctx context.Context, q *gen.Queries, rail, environment, accountID string) (RailMerchantAccountIdentity, bool, error) {
	row, err := q.ResolvePSPOwnerByRailIdentity(ctx, gen.ResolvePSPOwnerByRailIdentityParams{
		Rail:        rail,
		Environment: &environment,
		AccountID:   accountID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RailMerchantAccountIdentity{}, false, nil
	}
	if err != nil {
		return RailMerchantAccountIdentity{}, false, err
	}
	if row.ID == nil || row.MerchantID == nil {
		return RailMerchantAccountIdentity{}, false, nil
	}
	return RailMerchantAccountIdentity{
		ID:          *row.ID,
		MerchantID:  merchant.ID(*row.MerchantID),
		Rail:        derefString(row.Rail),
		Environment: derefString(row.Environment),
		AccountID:   derefString(row.AccountID),
	}, true, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// LiveRailPresence is the TRI-state answer to "does a live PSP exist on this
// rail, anywhere in the catalog?". The zero value is Unknown so a dropped or
// defaulted result fails closed.
type LiveRailPresence int

const (
	// LiveRailUnknown means the probe proved nothing. Callers MUST treat it
	// exactly as they treat LiveRailPresent.
	LiveRailUnknown LiveRailPresence = iota
	LiveRailPresent
	LiveRailAbsent
)

func (p LiveRailPresence) String() string {
	switch p {
	case LiveRailPresent:
		return "present"
	case LiveRailAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

// ProbeLiveRailPSPs reports whether ANY merchant declares a PSP on rail with
// environment=live. Deliberately cross-merchant: webhook ingestion has no
// merchant yet. It walks the control-plane merchant directory (a global,
// non-RLS table) and asks each merchant INSIDE ITS OWN RLS scope, so the answer
// is the same whether or not the connected role enforces RLS.
//
// SEC-19: the predecessor ran one no-GUC `EXISTS` on the base pool. psps FORCEs
// RLS, so under the unprivileged app role — mandatory outside development — the
// policy predicate is NULL and the query returns zero rows AND no error. The
// probe therefore reported "no live accounts" everywhere it mattered, silently
// disarming the guard built on it.
//
// Cost is O(merchants) small transactions, so this is for gate decisions on a
// cold path (the CCBill dev-allowlist gate), not per-event work.
func (s *Service) ProbeLiveRailPSPs(ctx context.Context, rail string) (LiveRailPresence, error) {
	if s == nil || s.pool == nil {
		return LiveRailUnknown, errors.New("merchants: pgx pool is required")
	}
	rail = normalizeProviderSecretType(rail)
	if rail == "" {
		return LiveRailUnknown, errors.New("merchants: rail is required")
	}
	ids, err := s.allMerchantIDs(ctx)
	if err != nil {
		return LiveRailUnknown, fmt.Errorf("merchants: list merchants for live %s psp probe: %w", rail, err)
	}
	if len(ids) == 0 {
		// An empty directory is indistinguishable from a directory read we were
		// not allowed to make — prove nothing rather than claim absence.
		return LiveRailUnknown, nil
	}
	live := "live"
	for _, id := range ids {
		found := false
		if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			n, err := gen.New(tx).CountPSPsForRailEnvironment(ctx, gen.CountPSPsForRailEnvironmentParams{
				MerchantID:  uuid.UUID(id),
				Rail:        rail,
				Environment: &live,
			})
			found = n > 0
			return err
		}); err != nil {
			return LiveRailUnknown, fmt.Errorf("merchants: probe live %s psps for merchant %s: %w", rail, id, err)
		}
		if found {
			return LiveRailPresent, nil
		}
	}
	return LiveRailAbsent, nil
}

// allMerchantIDs lists every merchant, INCLUDING soft-deleted ones — their psps
// rows survive the tombstone and still make a deployment "live".
// openrails.merchants is a global control-plane table (no RLS), so this read is
// legitimate on the base pool.
func (s *Service) allMerchantIDs(ctx context.Context) ([]merchant.ID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM openrails.merchants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []merchant.ID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, merchant.ID(id))
	}
	return ids, rows.Err()
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

// CountActivePSPsForRail is how many PSPs the merchant has ACTIVE for new work
// on a rail/environment. More than one is a supported state that only warns at
// credential-resolution time (the newest wins), but it is decisive for the pull
// plane: a pull arms from exactly ONE PSP, so a rail with N>1 active PSPs is
// only partially covered and its roster can never prove absence for the
// siblings it did not read (#841).
func (s *Service) CountActivePSPsForRail(ctx context.Context, id merchant.ID, rail, environment string) (int, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return 0, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return 0, fmt.Errorf("provider account environment must be live or test")
	}
	rail = normalizeProviderSecretType(rail)
	var count int64
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		count, e = gen.New(tx).CountActivePSPsForNewWork(ctx, gen.CountActivePSPsForNewWorkParams{
			MerchantID:  id.UUID(),
			Rail:        rail,
			Environment: &environment,
		})
		return e
	})
	if err != nil {
		return 0, fmt.Errorf("count active provider accounts %s/%s: %w", rail, environment, err)
	}
	return int(count), nil
}
