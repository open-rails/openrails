package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PaymentProviderCredentialStatus is the redacted credential view returned to
// merchant admins. It never contains plaintext.
type PaymentProviderCredentialStatus struct {
	Configured      bool       `json:"configured"`
	LastValidatedAt *time.Time `json:"last_validated_at,omitempty"`
	// RotationVersion is the or#812 cross-node cutover watermark: the secret
	// version this credential reached at its last rotation through this API.
	// Every node refuses to serve an OLDER version from cache, so a rotation is
	// deployment-wide the moment this number moves. 0 = the credential was
	// never rotated through this API (manifest-armed or pre-or#812).
	RotationVersion int `json:"rotation_version,omitempty"`
}

// PaymentProviderConfig is one merchant-owned payment-provider account.
type PaymentProviderConfig struct {
	ID              uuid.UUID                                  `json:"id"`
	Rail            string                                     `json:"rail"`
	Environment     string                                     `json:"environment"`
	AccountID       string                                     `json:"account_id"`
	Archived        bool                                       `json:"archived"`
	Drained         bool                                       `json:"drained"`
	OpenObligations int64                                      `json:"open_obligations"`
	PublicConfig    map[string]string                          `json:"public_config,omitempty"`
	Credentials     map[string]PaymentProviderCredentialStatus `json:"credentials"`
	FirstSeenAt     time.Time                                  `json:"first_seen_at"`
	LastVerifiedAt  *time.Time                                 `json:"last_validated_at,omitempty"`
	ReplacedAt      *time.Time                                 `json:"replaced_at,omitempty"`
	CreatedAt       time.Time                                  `json:"created_at"`
	UpdatedAt       time.Time                                  `json:"updated_at"`
}

// PaymentProviderDefinition describes one merchant-configurable provider from
// the rail registry. CredentialKeys contains only merchant-writable secrets.
type PaymentProviderDefinition struct {
	Rail           string   `json:"rail"`
	DisplayName    string   `json:"display_name"`
	CredentialKeys []string `json:"credential_keys"`
}

// UpsertPaymentProviderConfigRequest creates or replaces one provider account.
// There is no `environment` field (#882): a deployment is all-test or all-live,
// so the environment is derived from the deployment's test_mode posture.
type UpsertPaymentProviderConfigRequest struct {
	Enabled      *bool             `json:"enabled"`
	AccountID    string            `json:"account_id"`
	PublicConfig map[string]string `json:"public_config"`
	Credentials  map[string]string `json:"credentials"`
	// LegacyEnvironment stays bound ONLY so a caller still sending `environment`
	// is refused instead of silently ignored (#882).
	LegacyEnvironment string `json:"environment,omitempty"`
}

type railMerchantAccountEvidence struct {
	PublicConfig         map[string]string `json:"public_config,omitempty"`
	CredentialsValidated bool              `json:"credentials_validated,omitempty"`
	// CredentialVersions is the or#812 cross-node rotation watermark: the
	// Secret.Version each credential key reached at its last rotation. It rides
	// the PSP row because every credential resolution already re-reads that row
	// live from the shared DB, so the floor costs no extra query and reaches
	// every node the instant the rotation commits.
	CredentialVersions map[string]int `json:"credential_versions,omitempty"`
}

// PaymentProviderDefinitions returns every merchant-configurable provider in
// registry order.
func PaymentProviderDefinitions() []PaymentProviderDefinition {
	descriptors := rails.All()
	definitions := make([]PaymentProviderDefinition, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if !descriptor.HasRailMerchantAccounts {
			continue
		}
		credentialKeys := rails.MerchantCredentialKeyNames(descriptor.Rail)
		if credentialKeys == nil {
			credentialKeys = []string{}
		}
		definitions = append(definitions, PaymentProviderDefinition{
			Rail:           string(descriptor.Rail),
			DisplayName:    descriptor.DisplayName,
			CredentialKeys: credentialKeys,
		})
	}
	return definitions
}

// ListPaymentProviderConfigs returns provider-account configs for a merchant.
func (s *Service) ListPaymentProviderConfigs(ctx context.Context, id merchant.ID, rail, environment, status string) ([]PaymentProviderConfig, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	rail = normalizeProviderSecretType(rail)
	if strings.TrimSpace(environment) == "" {
		environment = s.providerEnvironment // deployment posture (#681)
	} else if environment = normalizeProviderSecretEnvironment(environment); environment == "" {
		return nil, errors.New("merchants: provider environment must be live or test")
	}
	status = strings.ToLower(strings.TrimSpace(status))
	// or#893: the lifecycle filter has ONE vocabulary. It used to accept four
	// synonyms for each state and return an EMPTY list for anything else — a
	// typo read as "this merchant has no PSPs", which is the worst possible
	// answer to a capability question.
	switch status {
	case "", pspLifecycleAll, pspLifecycleActive, pspLifecycleArchived:
	default:
		return nil, fmt.Errorf("merchants: unknown status %q (use %q, %q, or omit for all)", status, pspLifecycleActive, pspLifecycleArchived)
	}

	var rows []gen.OpenrailsPsp
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var providerFilter *string
		if rail != "" {
			providerFilter = &rail
		}
		var err error
		rows, err = gen.New(tx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{
			MerchantID: id.UUID(),
			Rail:       providerFilter,
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	statuses, err := s.ListSecretStatuses(ctx, id)
	if err != nil {
		return nil, err
	}
	accountIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		accountIDs = append(accountIDs, row.ID)
	}
	openObligations, err := s.pspOpenObligations(ctx, id, accountIDs)
	if err != nil {
		return nil, err
	}
	out := make([]PaymentProviderConfig, 0, len(rows))
	for _, row := range rows {
		if environment != "" && row.Environment != environment {
			continue
		}
		if !pspLifecycleMatches(row.Archived, status) {
			continue
		}
		cfg := paymentProviderConfigFromRow(row, statuses)
		cfg.applyOpenObligations(openObligations[row.ID])
		out = append(out, cfg)
	}
	return out, nil
}

// GetPaymentProviderConfig returns the active config for provider/env.
func (s *Service) GetPaymentProviderConfig(ctx context.Context, id merchant.ID, rail, environment string) (PaymentProviderConfig, error) {
	if strings.TrimSpace(environment) == "" {
		environment = s.providerEnvironment // deployment posture (#681)
	} else if environment = normalizeProviderSecretEnvironment(environment); environment == "" {
		return PaymentProviderConfig{}, errors.New("merchants: provider environment must be live or test")
	}
	items, err := s.ListPaymentProviderConfigs(ctx, id, rail, environment, pspLifecycleActive)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	if len(items) > 0 {
		return items[len(items)-1], nil
	}
	return PaymentProviderConfig{}, ErrSecretNotFound
}

// UpsertPaymentProviderConfig validates credentials first, then stores the
// provider account and scoped secrets.
func (s *Service) UpsertPaymentProviderConfig(ctx context.Context, id merchant.ID, rail string, req UpsertPaymentProviderConfigRequest) (PaymentProviderConfig, error) {
	if s == nil || s.pool == nil || s.secrets == nil {
		return PaymentProviderConfig{}, errors.New("merchants: provider config storage unavailable")
	}
	rail = normalizeProviderSecretType(rail)
	if !supportedPaymentProvider(rail) {
		return PaymentProviderConfig{}, fmt.Errorf("merchants: unsupported payment rail %q", rail)
	}
	if strings.TrimSpace(req.LegacyEnvironment) != "" {
		return PaymentProviderConfig{}, fmt.Errorf("merchants: `environment` was removed (#882): the environment is derived from the deployment's test_mode (currently %q) — drop the field", s.providerEnvironment)
	}
	environment := s.providerEnvironment // derived from test_mode (#681/#882)
	accountID := strings.TrimSpace(req.AccountID)
	if accountID == "" {
		return PaymentProviderConfig{}, fmt.Errorf("merchants: provider account_id required")
	}
	if err := s.refuseLiveNMIUnderTestMode(ctx, id, rail, environment, accountID, req.Credentials); err != nil {
		return PaymentProviderConfig{}, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// secretNames maps the scoped secret NAME to its value; secretKeys maps the
	// same name back to the normalized credential KEY, which is what the
	// version floor on the PSP row is recorded under.
	secretNames := make(map[string]string, len(req.Credentials))
	secretKeys := make(map[string]string, len(req.Credentials))
	probeCredentials := make(map[string]string, len(req.Credentials))
	credentialsValidated := false
	for key, value := range req.Credentials {
		normalizedKey, err := NormalizePSPSecretKey(rail, key)
		if err != nil {
			return PaymentProviderConfig{}, err
		}
		name, err := PSPSecretName(rail, environment, accountID, key)
		if err != nil {
			return PaymentProviderConfig{}, err
		}
		if err := s.ValidateCredential(ctx, id, name, value, nil); err != nil {
			return PaymentProviderConfig{}, err
		}
		if rail == "stripe" && normalizedKey == "secret_key" {
			credentialsValidated = true
		}
		secretNames[name] = value
		secretKeys[name] = normalizedKey
		probeCredentials[normalizedKey] = value
	}
	// ROTATION ORDER (or#812). The live probe runs FIRST and on the credentials
	// SUPPLIED IN THIS REQUEST, so a bad new credential fails here — before any
	// secret is written and before any version floor moves. The old credential
	// stays exactly as it was and keeps serving on every node.
	probed, err := s.probePaymentProviderCredentials(ctx, id, rail, environment, accountID, probeCredentials)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	credentialsValidated = credentialsValidated || probed
	var lastVerifiedAt *time.Time
	if credentialsValidated {
		now := time.Now().UTC()
		lastVerifiedAt = &now
	}

	// Secrets are written BEFORE the PSP row so the version floor never becomes
	// visible to another node ahead of the value it demands.
	credentialVersions := make(map[string]int, len(secretNames))
	for name, value := range secretNames {
		sec, err := s.PutCredential(ctx, id, name, value)
		if err != nil {
			return PaymentProviderConfig{}, err
		}
		if key := secretKeys[name]; key != "" && sec.Version > 0 {
			credentialVersions[NormalizeCredentialVersionKey(key)] = sec.Version
		}
	}
	row, err := s.upsertRailMerchantAccount(ctx, id, rail, environment, accountID, enabled, req.PublicConfig, credentialsValidated, lastVerifiedAt, credentialVersions)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	statuses, err := s.ListSecretStatuses(ctx, id)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	cfg := paymentProviderConfigFromRow(row, statuses)
	openObligations, err := s.pspOpenObligations(ctx, id, []uuid.UUID{row.ID})
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	cfg.applyOpenObligations(openObligations[row.ID])
	return cfg, nil
}

// DeletePaymentProviderConfig archives one provider environment. Scoped
// credentials remain so existing obligations and inbound webhooks can drain.
func (s *Service) DeletePaymentProviderConfig(ctx context.Context, id merchant.ID, rail, environment string) (PaymentProviderConfig, error) {
	if s == nil || s.pool == nil || s.secrets == nil {
		return PaymentProviderConfig{}, errors.New("merchants: provider config storage unavailable")
	}
	current, err := s.GetPaymentProviderConfig(ctx, id, rail, environment)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	row, err := s.disableRailMerchantAccount(ctx, id, current.ID)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	statuses, err := s.ListSecretStatuses(ctx, id)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	cfg := paymentProviderConfigFromRow(row, statuses)
	openObligations, err := s.pspOpenObligations(ctx, id, []uuid.UUID{row.ID})
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	cfg.applyOpenObligations(openObligations[row.ID])
	return cfg, nil
}

func (s *Service) upsertRailMerchantAccount(ctx context.Context, id merchant.ID, rail, environment, accountID string, enabled bool, publicConfig map[string]string, credentialsValidated bool, lastVerifiedAt *time.Time, credentialVersions map[string]int) (gen.OpenrailsPsp, error) {
	// #650: reject a cross-merchant claim with a clear error before the upsert
	// (which would otherwise fail with an opaque unique-violation under RLS).
	queries := gen.New(s.pool)
	if err := AssertPSPUnowned(ctx, queries, id.UUID(), rail, environment, accountID); err != nil {
		return gen.OpenrailsPsp{}, err
	}
	archived := !enabled
	// #662: derive the id from the global natural key and store the SAME
	// normalized (rail, environment, account_id) the id is hashed from, so the
	// id corresponds 1:1 to the unique index.
	railAcctID, nRail, nEnv, nAccount := PSPNaturalKey(rail, environment, accountID)
	// Always read the existing row: fields this request omits are carried
	// forward, and the or#812 credential-version floors of credentials it did
	// not rotate must survive.
	var existingEvidenceRaw []byte
	existing, err := queries.GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{
		Rail:        nRail,
		Environment: &nEnv,
		AccountID:   nAccount,
	})
	switch {
	case err == nil:
		existingEvidenceRaw = existing.Evidence
		existingEvidence := unmarshalProviderEvidence(existing.Evidence)
		if len(publicConfig) == 0 {
			publicConfig = existingEvidence.PublicConfig
		}
		if !credentialsValidated && existingEvidence.CredentialsValidated {
			credentialsValidated = true
			lastVerifiedAt = existing.LastVerifiedAt
		}
		credentialVersions = mergeCredentialVersions(existingEvidence.CredentialVersions, credentialVersions)
	case !errors.Is(err, pgx.ErrNoRows):
		return gen.OpenrailsPsp{}, fmt.Errorf("merchants: load existing provider config: %w", err)
	default:
		credentialVersions = mergeCredentialVersions(nil, credentialVersions)
	}
	evidence, err := marshalProviderEvidence(existingEvidenceRaw, publicConfig, credentialsValidated, credentialVersions)
	if err != nil {
		return gen.OpenrailsPsp{}, err
	}
	var row gen.OpenrailsPsp
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).UpsertPSP(ctx, gen.UpsertPSPParams{
			ID:             railAcctID,
			MerchantID:     id.UUID(),
			Rail:           nRail,
			Environment:    &nEnv,
			AccountID:      nAccount,
			Archived:       &archived,
			Evidence:       evidence,
			LastVerifiedAt: lastVerifiedAt,
		})
		return err
	})
	return row, err
}

func (s *Service) disableRailMerchantAccount(ctx context.Context, id merchant.ID, accountID uuid.UUID) (gen.OpenrailsPsp, error) {
	var row gen.OpenrailsPsp
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
				UPDATE openrails.psps
				   SET archived = true,
				       replaced_at = COALESCE(replaced_at, now()),
				       updated_at = now()
				 WHERE id = $1 AND merchant_id = $2
				RETURNING id, merchant_id, rail, environment, account_id, key, evidence, first_seen_at, last_verified_at, replaced_at, created_at, updated_at, archived
			`, accountID, id.UUID()).Scan(
			&row.ID, &row.MerchantID, &row.Rail, &row.Environment, &row.AccountID,
			&row.Key, &row.Evidence,
			&row.FirstSeenAt, &row.LastVerifiedAt, &row.ReplacedAt, &row.CreatedAt, &row.UpdatedAt,
			&row.Archived,
		)
	})
	return row, err
}

func (s *Service) pspOpenObligations(ctx context.Context, id merchant.ID, accountIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	out := make(map[uuid.UUID]int64, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH target AS (SELECT unnest($1::uuid[]) AS id)
			SELECT target.id,
			       (
			         SELECT count(*)::bigint
			           FROM openrails.subscriptions sub
			          WHERE sub.merchant_id = $2::uuid
			            AND sub.psp_id = target.id
			            AND sub.status IN ('active', 'pending', 'past_due')
			       ) +
			       (
			         SELECT count(*)::bigint
			           FROM openrails.payments payment
			          WHERE payment.merchant_id = $2::uuid
			            AND payment.psp_id = target.id
			            AND payment.status = 'pending'
			       ) +
			       (
			         SELECT count(*)::bigint
			           FROM openrails.rail_intents intent
			          WHERE intent.merchant_id = $2::uuid
			            AND intent.psp_id = target.id
			            AND intent.status IN ('pending', 'in_flight', 'failed_retryable', 'unknown_needs_verify')
			       ) AS open_obligations
			  FROM target
		`, accountIDs, id.UUID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var accountID uuid.UUID
			var count int64
			if err := rows.Scan(&accountID, &count); err != nil {
				return err
			}
			out[accountID] = count
		}
		return rows.Err()
	})
	if isUndefinedTable(err) {
		return out, nil
	}
	return out, err
}

func (c *PaymentProviderConfig) applyOpenObligations(count int64) {
	c.OpenObligations = count
	c.Drained = c.Archived && count == 0
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

func paymentProviderConfigFromRow(row gen.OpenrailsPsp, statuses []MerchantSecretStatus) PaymentProviderConfig {
	evidence := unmarshalProviderEvidence(row.Evidence)
	configured := map[string]struct{}{}
	for _, st := range statuses {
		if st.Configured {
			configured[cleanSecretName(st.Name)] = struct{}{}
		}
	}
	lastVerifiedAt := row.LastVerifiedAt
	if !evidence.CredentialsValidated || !providerValidationCredentialsConfigured(row, configured) {
		lastVerifiedAt = nil
	}
	credentials := make(map[string]PaymentProviderCredentialStatus)
	for _, key := range paymentProviderCredentialKeys(row.Rail) {
		name, err := PSPSecretName(row.Rail, row.Environment, row.AccountID, key)
		if err != nil {
			continue
		}
		_, ok := configured[name]
		var validatedAt *time.Time
		if ok {
			validatedAt = credentialValidatedAt(row.Rail, key, lastVerifiedAt)
		}
		credentials[key] = PaymentProviderCredentialStatus{
			Configured:      ok,
			LastValidatedAt: validatedAt,
			RotationVersion: evidence.CredentialVersions[NormalizeCredentialVersionKey(key)],
		}
	}
	return PaymentProviderConfig{
		ID:             row.ID,
		Rail:           row.Rail,
		Environment:    row.Environment,
		AccountID:      row.AccountID,
		Archived:       row.Archived,
		PublicConfig:   evidence.PublicConfig,
		Credentials:    credentials,
		FirstSeenAt:    row.FirstSeenAt,
		LastVerifiedAt: lastVerifiedAt,
		ReplacedAt:     row.ReplacedAt,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func providerValidationCredentialsConfigured(row gen.OpenrailsPsp, configured map[string]struct{}) bool {
	var requiredKeys []string
	switch row.Rail {
	case "stripe":
		requiredKeys = []string{"secret_key"}
	case "nmi":
		requiredKeys = []string{"security_key"}
	case "ccbill":
		requiredKeys = []string{"datalink_username", "datalink_password"}
	default:
		return false
	}
	for _, key := range requiredKeys {
		name, err := PSPSecretName(row.Rail, row.Environment, row.AccountID, key)
		if err != nil {
			return false
		}
		if _, ok := configured[name]; !ok {
			return false
		}
	}
	return true
}

// PSP lifecycle filter vocabulary. A PSP row is active or archived; there is no
// third state and no synonym for either.
const (
	pspLifecycleActive   = "active"
	pspLifecycleArchived = "archived"
	pspLifecycleAll      = "all" // explicit spelling of the empty filter
)

func pspLifecycleMatches(archived bool, status string) bool {
	switch status {
	case pspLifecycleActive:
		return !archived
	case pspLifecycleArchived:
		return archived
	default: // "" / "all" — validated by the caller
		return true
	}
}

// supportedPaymentProvider is registry-backed (#669): the rail participates in
// the provider-account catalog.
func supportedPaymentProvider(provider string) bool {
	return rails.SupportsRailMerchantAccounts(models.Rail(provider))
}

// paymentProviderCredentialKeys returns the MERCHANT-visible credential slots
// (registry-backed, #669). Operator-only secrets (solana private_key) are
// deliberately absent from the merchant credential-status view.
func paymentProviderCredentialKeys(provider string) []string {
	return rails.MerchantCredentialKeyNames(models.Rail(provider))
}

func credentialValidatedAt(rail, key string, validatedAt *time.Time) *time.Time {
	if validatedAt == nil {
		return nil
	}
	switch rail {
	case "stripe":
		if key == "secret_key" {
			return validatedAt
		}
	case "nmi":
		if key == "security_key" {
			return validatedAt
		}
	case "ccbill":
		if key == "datalink_username" || key == "datalink_password" {
			return validatedAt
		}
	}
	return nil
}

// marshalProviderEvidence overlays the fields this API owns onto the evidence
// document already on the row. Evidence is a shared, free-form JSONB — the
// manifest path also writes `source`, `signer` and `settings` there — so an API
// arm must merge, not replace, or a manifest-armed PSP loses its settings the
// first time an operator rotates a credential.
func marshalProviderEvidence(existing []byte, publicConfig map[string]string, credentialsValidated bool, credentialVersions map[string]int) ([]byte, error) {
	doc := map[string]json.RawMessage{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			doc = map[string]json.RawMessage{}
		}
	}
	set := func(key string, value any, keep bool) error {
		if !keep {
			delete(doc, key)
			return nil
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal provider config evidence %s: %w", key, err)
		}
		doc[key] = raw
		return nil
	}
	if err := set("public_config", publicConfig, len(publicConfig) > 0); err != nil {
		return nil, err
	}
	if err := set("credentials_validated", credentialsValidated, credentialsValidated); err != nil {
		return nil, err
	}
	if err := set("credential_versions", credentialVersions, len(credentialVersions) > 0); err != nil {
		return nil, err
	}
	if len(doc) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal provider config evidence: %w", err)
	}
	return b, nil
}

// mergeCredentialVersions carries forward the version floors of credentials
// this request did not touch and raises the floors it did. A floor NEVER goes
// backwards: a lower observed version means an out-of-order write, and the
// higher floor is the safe one (it only ever forces a re-read).
func mergeCredentialVersions(existing, rotated map[string]int) map[string]int {
	if len(existing) == 0 && len(rotated) == 0 {
		return nil
	}
	out := make(map[string]int, len(existing)+len(rotated))
	for k, v := range existing {
		if k = NormalizeCredentialVersionKey(k); k != "" && v > 0 {
			out[k] = v
		}
	}
	for k, v := range rotated {
		if k = NormalizeCredentialVersionKey(k); k != "" && v > out[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func unmarshalProviderEvidence(raw []byte) railMerchantAccountEvidence {
	if len(raw) == 0 {
		return railMerchantAccountEvidence{}
	}
	var out railMerchantAccountEvidence
	_ = json.Unmarshal(raw, &out)
	return out
}

// refuseLiveNMIUnderTestMode reinstates #348 at the MODE-2 (API) arm
// boundary — the #788 rail-layering refactor deleted the boot-time probe
// with no per-merchant replacement, so a test_mode deployment could arm real
// production NMI credentials with nothing to catch it. Under test_mode, an
// NMI account whose credentials belong to a LIVE gateway must never be
// armed: every charge through it would move real money while the deployment
// believes it is sandboxed. Non-NMI rails and non-test_mode deployments
// (s.providerEnvironment == "live") are untouched — probing on every arm in
// production would charge a real card on every credential rotation.
//
// Posture, preserved exactly from #348's original boot probe: only a
// conclusive "live" verdict refuses the arm. A probe error (offline dev, bad
// credentials, transport failure) is indeterminate and only warns — it is
// NEVER fail-closed, because network/credential noise is not evidence of a
// live account.
func (s *Service) refuseLiveNMIUnderTestMode(ctx context.Context, id merchant.ID, rail, environment, accountID string, credentials map[string]string) error {
	if rail != string(models.RailNMI) || s.providerEnvironment != "test" {
		return nil
	}
	name, err := PSPSecretName(rail, environment, accountID, "security_key")
	if err != nil {
		return err
	}
	securityKey := strings.TrimSpace(credentials["security_key"])
	if securityKey == "" && s.secrets != nil {
		// The arm may only be touching other fields (public_config, a second
		// credential) — resolve the EFFECTIVE key already on file so a
		// live account can't slip through by omitting security_key from this
		// particular request.
		if sec, gerr := s.secrets.Get(ctx, id, name); gerr == nil {
			securityKey = strings.TrimSpace(sec.Value)
		}
	}
	if securityKey == "" {
		return nil // unconfigured; nothing to verify
	}
	client, err := nmi.NewClient(accountID, &config.NMIProviderSettings{SecurityKey: securityKey}, true)
	if err != nil {
		return nil // never stricter than #348: a client that fails to construct cannot be probed
	}
	if s.nmiProbeV5BaseURL != "" {
		client.V5BaseURL = s.nmiProbeV5BaseURL
	}
	// Scoped by merchant + rail + environment + account so a cached verdict
	// never answers for a different merchant's account.
	cacheKey := id.String() + ":" + name
	decision := nmi.CheckTestModeArm(ctx, gen.New(s.pool), client, cacheKey)
	if decision.ProbeErr != nil {
		log.WithError(decision.ProbeErr).WithFields(log.Fields{
			"merchant_id": id.String(), "rail": rail, "account_id": accountID,
		}).Warn("payment provider arm: could not verify the NMI account is a sandbox account; proceeding, but confirm the credentials before relying on test_mode (#348)")
		return nil
	}
	if !decision.Refuse {
		return nil
	}
	if decision.Cached {
		return fmt.Errorf("merchants: rail %q account %q: PRODUCTION NMI credentials detected while test_mode is enabled — cached probe verdict 'live' from %s (within the %s cooldown; not re-probing); arm refused (use the sandbox account credentials, rotate the key, or unset test_mode) (#348)",
			rail, accountID, decision.CheckedAt.UTC().Format(time.RFC3339), nmi.ProbeVerdictCooldown)
	}
	return fmt.Errorf("merchants: rail %q account %q: PRODUCTION NMI credentials detected while test_mode is enabled — the account did not simulate the test-card probe, so real charges could occur; arm refused (use the sandbox account credentials, or unset test_mode) (#348)",
		rail, accountID)
}
