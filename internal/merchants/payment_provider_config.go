package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PaymentProviderCredentialStatus is the redacted credential view returned to
// merchant admins. It never contains plaintext.
type PaymentProviderCredentialStatus = openrails.PaymentProviderCredentialStatus

// ErrPaymentProviderNotFound reports that the merchant has no active provider
// account on the requested rail and environment.
var ErrPaymentProviderNotFound error = apperr.New(http.StatusNotFound, "payment_provider_not_found", "merchants: payment provider not configured")

// ErrPaymentProviderCredentialsRejected is the typed refusal (#983) for
// credentials the provider itself would not accept for this deployment posture.
var ErrPaymentProviderCredentialsRejected = apperr.New(http.StatusBadRequest, "payment_provider_credentials_rejected", "payment provider rejected the credentials")

// providerCredentialError types a provider-side credential rejection; any
// other probe outcome (transport, indeterminate) stays an internal failure.
func providerCredentialError(err error) error {
	if errors.Is(err, nmi.ErrCredentialsRejected) || errors.Is(err, nmi.ErrLiveCredentialsUnderTestMode) || errors.Is(err, nmi.ErrTestModeUnderLivePosture) || errors.Is(err, nmi.ErrSandboxEndpointUnderLive) {
		return fmt.Errorf("%w: %v", ErrPaymentProviderCredentialsRejected, err)
	}
	return err
}

// PaymentProviderConfig is one merchant-owned payment-PSP.

type PaymentProviderConfig = openrails.PaymentProviderConfig

// PaymentProviderDefinition describes one merchant-configurable provider from
// the rail registry. CredentialKeys contains only merchant-writable secrets.
type PaymentProviderDefinition = openrails.PaymentProviderDefinition

// UpsertPaymentProviderConfigRequest creates or replaces one PSP.
// There is no `environment` field (#882): a deployment is all-test or all-live,
// so the environment is derived from the deployment's test_mode posture.
type UpsertPaymentProviderConfigRequest = openrails.UpsertPaymentProviderParams

type pspEvidence struct {
	WebhookEndpointID    string               `json:"webhook_endpoint_id,omitempty"`
	RetiredCredentials   map[string]bool      `json:"retired_credentials,omitempty"`
	CredentialCustody    string               `json:"credential_custody,omitempty"`
	Revision             int64                `json:"configuration_revision,omitempty"`
	CredentialRefs       map[string]SecretRef `json:"credential_refs,omitempty"`
	PublicConfig         map[string]string    `json:"public_config,omitempty"`
	CredentialsValidated bool                 `json:"credentials_validated,omitempty"`
	// CredentialVersions is the per-slot logical rotation generation used to
	// fence qualification across value and custody changes. Published readers
	// use CredentialRefs for the independent backend version and exact name.
	// Legacy rows without references retain their original backend floors.
	CredentialVersions map[string]int `json:"credential_versions,omitempty"`
}

// PaymentProviderDefinitions returns every merchant-configurable provider in
// registry order.
func PaymentProviderDefinitions() []PaymentProviderDefinition {
	descriptors := rails.All()
	definitions := make([]PaymentProviderDefinition, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if !descriptor.HasPSPs {
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

// ListPaymentProviderConfigs returns PSP configs for a merchant.
func (s *Service) ListPaymentProviderConfigs(ctx context.Context, id merchant.ID, rail, environment, status string) ([]PaymentProviderConfig, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	rail = normalizeProviderSecretType(rail)
	if strings.TrimSpace(environment) == "" {
		environment = s.providerEnvironment // deployment posture (#681)
	} else if environment = normalizeProviderSecretEnvironment(environment); environment == "" {
		return nil, apperr.Invalidf("merchants: provider environment must be live or test")
	}
	status = strings.ToLower(strings.TrimSpace(status))
	// or#893: the lifecycle filter has ONE vocabulary. It used to accept four
	// synonyms for each state and return an EMPTY list for anything else — a
	// typo read as "this merchant has no PSPs", which is the worst possible
	// answer to a capability question.
	switch status {
	case "", pspLifecycleAll, pspLifecycleActive, pspLifecycleArchived:
	default:
		return nil, apperr.Invalidf("merchants: unknown status %q (use %q, %q, or omit for all)", status, pspLifecycleActive, pspLifecycleArchived)
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
		statuses, err := s.paymentProviderCredentialStatuses(ctx, id, row)
		if err != nil {
			return nil, err
		}
		cfg := paymentProviderConfigFromRow(row, statuses)
		applyOpenObligations(&cfg, openObligations[row.ID])
		out = append(out, cfg)
	}
	return out, nil
}

// GetPaymentProviderConfig returns the active config for provider/env.
func (s *Service) GetPaymentProviderConfig(ctx context.Context, id merchant.ID, rail, environment string) (PaymentProviderConfig, error) {
	if strings.TrimSpace(environment) == "" {
		environment = s.providerEnvironment // deployment posture (#681)
	} else if environment = normalizeProviderSecretEnvironment(environment); environment == "" {
		return PaymentProviderConfig{}, apperr.Invalidf("merchants: provider environment must be live or test")
	}
	items, err := s.ListPaymentProviderConfigs(ctx, id, rail, environment, pspLifecycleActive)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	if len(items) > 0 {
		return items[len(items)-1], nil
	}
	return PaymentProviderConfig{}, ErrPaymentProviderNotFound
}

// ErrPSPClaimUnproven refuses a merchant's first claim of a provider account
// whose credentials do not prove control of it (SEC-33). The deployment
// operator declares such accounts instead.
var ErrPSPClaimUnproven = apperr.New(http.StatusForbidden, "psp_claim_requires_proof", "provider account claims require credentials that prove control of the account, or operator declaration")

// claimNeedsProof lists rails whose inbound events route by account id.
func claimNeedsProof(rail string) bool {
	return rail == "stripe" || rail == "nmi" || rail == "ccbill"
}

// UpsertPaymentProviderConfig validates credentials first, then stores the
// PSP and scoped secrets. A merchant's first claim of an account must prove
// control through a successful credential probe.
func (s *Service) UpsertPaymentProviderConfig(ctx context.Context, id merchant.ID, rail string, req UpsertPaymentProviderConfigRequest) (PaymentProviderConfig, error) {
	return s.upsertPaymentProviderConfig(ctx, id, rail, req, false)
}

// OperatorUpsertPaymentProviderConfig is the operator's approved claim: the
// deployment operator vouches for account ownership.
func (s *Service) OperatorUpsertPaymentProviderConfig(ctx context.Context, id merchant.ID, rail string, req UpsertPaymentProviderConfigRequest) (PaymentProviderConfig, error) {
	return s.upsertPaymentProviderConfig(ctx, id, rail, req, true)
}

func (s *Service) upsertPaymentProviderConfig(ctx context.Context, id merchant.ID, rail string, req UpsertPaymentProviderConfigRequest, operatorApproved bool) (PaymentProviderConfig, error) {
	if s == nil || s.pool == nil || s.secrets == nil {
		return PaymentProviderConfig{}, errors.New("merchants: provider config storage unavailable")
	}
	rail = normalizeProviderSecretType(rail)
	if !supportedPaymentProvider(rail) {
		return PaymentProviderConfig{}, apperr.Invalidf("merchants: unsupported payment rail %q", rail)
	}
	if strings.TrimSpace(req.LegacyEnvironment) != "" {
		return PaymentProviderConfig{}, apperr.Invalidf("merchants: `environment` was removed (#882): the environment is derived from the deployment's test_mode (currently %q) — drop the field", s.providerEnvironment)
	}
	for key, value := range req.PublicConfig {
		if key != "publishable_key" && key != "tokenization_key" {
			return PaymentProviderConfig{}, apperr.Invalidf("unsupported public provider configuration field")
		}
		if key == "publishable_key" && (rail != "stripe" || !strings.HasPrefix(value, "pk_")) {
			return PaymentProviderConfig{}, apperr.Invalidf("invalid public publishable key")
		}
		if key == "tokenization_key" && rail != "nmi" {
			return PaymentProviderConfig{}, apperr.Invalidf("invalid public tokenization key")
		}
		for _, secret := range req.Credentials {
			if value != "" && value == secret {
				return PaymentProviderConfig{}, apperr.Invalidf("secret credentials cannot be stored as public configuration")
			}
		}
	}
	environment := s.providerEnvironment // derived from test_mode (#681/#882)
	accountID := strings.TrimSpace(req.AccountID)
	if accountID == "" {
		return PaymentProviderConfig{}, apperr.Invalidf("merchants: provider account_id required")
	}
	if req.OperationID == uuid.Nil || req.ExpectedRevision == nil || *req.ExpectedRevision < 0 {
		return PaymentProviderConfig{}, apperr.Invalidf("operation_id and nonnegative expected_revision are required")
	}
	if receipt, completed, err := s.replayProviderCredentialPublication(ctx, id, rail, environment, accountID, req); err != nil {
		return PaymentProviderConfig{}, err
	} else if completed {
		return s.paymentProviderConfigWithObligations(ctx, id, receipt)
	}
	if len(req.Credentials) > 0 && !CanStageCredentials(s.secrets) {
		return PaymentProviderConfig{}, apperr.New(http.StatusMethodNotAllowed, "credential_source_read_only", "provider credential source has no writable durable custody")
	}
	claiming := false
	if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			return err
		}
		if err := AssertPSPUnowned(ctx, q, id.UUID(), rail, environment, accountID); err != nil {
			return err
		}
		row, err := q.GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{MerchantID: id.UUID(), Rail: rail, Environment: &environment, AccountID: accountID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		claiming = errors.Is(err, pgx.ErrNoRows)
		if err == nil {
			custody := unmarshalProviderEvidence(row.Evidence).CredentialCustody
			if custody != "" && custody != SecretCustodyIdentity(s.secrets) {
				return ErrCredentialCustodyTransitionRequired
			}
		}
		return nil
	}); err != nil {
		return PaymentProviderConfig{}, err
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
		if rail == "stripe" && normalizedKey == "secret_key" {
			if err := validateSecretValueLocal(name, value); err != nil {
				return PaymentProviderConfig{}, err
			}
		} else if err := s.ValidateCredential(ctx, id, name, value, nil); err != nil {
			return PaymentProviderConfig{}, err
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
	if claiming && !probed && !operatorApproved && claimNeedsProof(rail) {
		return PaymentProviderConfig{}, ErrPSPClaimUnproven
	}
	var lastVerifiedAt *time.Time
	if credentialsValidated {
		now := time.Now().UTC()
		lastVerifiedAt = &now
	}

	row, err := s.publishProviderCredentials(ctx, id, rail, environment, accountID, enabled, req, secretNames, secretKeys, credentialsValidated, lastVerifiedAt, credentialTransitionPublication{RetireWebhookOverlap: req.RetireWebhookOverlap})
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	statuses, err := s.paymentProviderCredentialStatuses(ctx, id, row)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	cfg := paymentProviderConfigFromRow(row, statuses)
	openObligations, err := s.pspOpenObligations(ctx, id, []uuid.UUID{row.ID})
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	applyOpenObligations(&cfg, openObligations[row.ID])
	return cfg, nil
}

// ArchivePaymentProviderAccountRequest is the body of the explicit per-account
// archive (#655/#656).
type ArchivePaymentProviderAccountRequest struct {
	// AllowLast must be explicit to archive the ONLY active account on the
	// rail: afterwards new checkout on that rail is refused until another
	// account is armed. Absent, the archive fails closed.
	AllowLast bool `json:"allow_last"`
}

// ProviderAccountRef identifies one PSP row inside a lifecycle refusal.
type ProviderAccountRef struct {
	ID        uuid.UUID `json:"id"`
	AccountID string    `json:"account_id"`
}

// ErrPaymentProviderAccountNotFound reports a PSP id the merchant does not own
// on the named rail.
var ErrPaymentProviderAccountNotFound = errors.New("merchants: payment provider account not found")

// LastActiveProviderAccountError refuses to archive the only active account on
// a rail without the explicit AllowLast override.
type LastActiveProviderAccountError struct {
	Rail        string
	Environment string
	Account     ProviderAccountRef
}

func (e *LastActiveProviderAccountError) Error() string {
	return fmt.Sprintf("merchants: %s account %s is the only active account on rail %q (%s); pass allow_last=true to archive it and refuse new checkout on the rail", e.Rail, e.Account.AccountID, e.Rail, e.Environment)
}

// MultipleActiveProviderAccountsError refuses the rail-level archive when the
// rail selector is ambiguous: the caller must name the PSP id.
type MultipleActiveProviderAccountsError struct {
	Rail        string
	Environment string
	Accounts    []ProviderAccountRef
}

func (e *MultipleActiveProviderAccountsError) Error() string {
	ids := make([]string, 0, len(e.Accounts))
	for _, account := range e.Accounts {
		ids = append(ids, account.ID.String())
	}
	return fmt.Sprintf("merchants: rail %q (%s) has %d active accounts (%s); archive one by its psp id", e.Rail, e.Environment, len(e.Accounts), strings.Join(ids, ", "))
}

// ArchivePaymentProviderAccount archives exactly the named account (#655
// lifecycle, #656 emergency step 3). It never contacts the provider — a
// terminated or dark account must still be archivable — and never touches the
// stored credentials, so existing obligations and inbound webhooks keep
// draining. Archiving an already-archived account is a no-op that returns the
// current row. The only active account on the rail is refused unless
// req.AllowLast is set.
func (s *Service) ArchivePaymentProviderAccount(ctx context.Context, id merchant.ID, rail string, pspID uuid.UUID, req ArchivePaymentProviderAccountRequest) (PaymentProviderConfig, error) {
	if s == nil || s.pool == nil {
		return PaymentProviderConfig{}, errors.New("merchants: provider config storage unavailable")
	}
	rail = normalizeProviderSecretType(rail)
	if !supportedPaymentProvider(rail) {
		return PaymentProviderConfig{}, apperr.Invalidf("merchants: unsupported payment rail %q", rail)
	}
	if pspID == uuid.Nil {
		return PaymentProviderConfig{}, apperr.Invalidf("merchants: provider account id required")
	}
	row, err := s.archivePSP(ctx, id, rail, pspID, req.AllowLast)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	return s.paymentProviderConfigWithObligations(ctx, id, row)
}

// DeletePaymentProviderConfig is the rail-level archive: it archives the rail's
// single active account and fails closed when the rail has more than one
// (MultipleActiveProviderAccountsError) — the caller must then name the PSP id
// through ArchivePaymentProviderAccount. Credentials remain so existing
// obligations and inbound webhooks can drain; no provider call is made.
func (s *Service) DeletePaymentProviderConfig(ctx context.Context, id merchant.ID, rail, environment string) (PaymentProviderConfig, error) {
	if s == nil || s.pool == nil {
		return PaymentProviderConfig{}, errors.New("merchants: provider config storage unavailable")
	}
	rail = normalizeProviderSecretType(rail)
	if !supportedPaymentProvider(rail) {
		return PaymentProviderConfig{}, apperr.Invalidf("merchants: unsupported payment rail %q", rail)
	}
	if strings.TrimSpace(environment) == "" {
		environment = s.providerEnvironment
	} else if environment = normalizeProviderSecretEnvironment(environment); environment == "" {
		return PaymentProviderConfig{}, apperr.Invalidf("merchants: provider environment must be live or test")
	}
	row, err := s.archiveSoleActivePSP(ctx, id, rail, environment)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	return s.paymentProviderConfigWithObligations(ctx, id, row)
}

func (s *Service) paymentProviderConfigWithObligations(ctx context.Context, id merchant.ID, row gen.OpenrailsPsp) (PaymentProviderConfig, error) {
	statuses, err := s.paymentProviderCredentialStatuses(ctx, id, row)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	cfg := paymentProviderConfigFromRow(row, statuses)
	openObligations, err := s.pspOpenObligations(ctx, id, []uuid.UUID{row.ID})
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	applyOpenObligations(&cfg, openObligations[row.ID])
	return cfg, nil
}

func (s *Service) upsertPSP(ctx context.Context, id merchant.ID, rail, environment, accountID string, enabled bool, publicConfig map[string]string, credentialsValidated bool, lastVerifiedAt *time.Time, credentialVersions map[string]int) (gen.OpenrailsPsp, error) {
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
		MerchantID:  id.UUID(),
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
	key := existing.Key
	if key == nil {
		key = &nRail
	}
	var row gen.OpenrailsPsp
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).UpsertPSP(ctx, gen.UpsertPSPParams{
			ID:             railAcctID,
			Key:            key,
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

const pspRowColumns = `id, merchant_id, rail, environment, account_id, key, evidence, first_seen_at, last_verified_at, replaced_at, created_at, updated_at, archived`

func scanPSPRow(row pgx.Row) (gen.OpenrailsPsp, error) {
	var out gen.OpenrailsPsp
	err := row.Scan(
		&out.ID, &out.MerchantID, &out.Rail, &out.Environment, &out.AccountID,
		&out.Key, &out.Evidence,
		&out.FirstSeenAt, &out.LastVerifiedAt, &out.ReplacedAt, &out.CreatedAt, &out.UpdatedAt,
		&out.Archived,
	)
	return out, err
}

// lockRailPSPs locks every PSP row of the merchant on (rail, environment) for
// the transaction, so two concurrent archives cannot each see the other as the
// remaining active account and leave the rail with none.
func lockRailPSPs(ctx context.Context, tx pgx.Tx, id merchant.ID, rail, environment string) ([]gen.OpenrailsPsp, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+pspRowColumns+`
		  FROM openrails.psps
		 WHERE merchant_id = $1 AND rail = $2 AND environment = $3
		 ORDER BY created_at, id
		   FOR UPDATE`, id.UUID(), rail, environment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []gen.OpenrailsPsp
	for rows.Next() {
		row, err := scanPSPRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func markPSPArchived(ctx context.Context, tx pgx.Tx, id merchant.ID, pspID uuid.UUID) (gen.OpenrailsPsp, error) {
	return scanPSPRow(tx.QueryRow(ctx, `
		UPDATE openrails.psps
		   SET archived = true,
             evidence=jsonb_set(COALESCE(evidence,'{}'::jsonb),'{configuration_revision}',to_jsonb(COALESCE((evidence->>'configuration_revision')::bigint,0)+1)),
		       replaced_at = COALESCE(replaced_at, now()),
		       updated_at = now()
		 WHERE id = $1 AND merchant_id = $2
		RETURNING `+pspRowColumns, pspID, id.UUID()))
}

// archivePSP archives the named account inside one transaction that holds the
// rail's rows locked (in one order, so concurrent archives serialize instead
// of deadlocking). An already-archived account is returned unchanged.
func (s *Service) archivePSP(ctx context.Context, id merchant.ID, rail string, pspID uuid.UUID, allowLast bool) (gen.OpenrailsPsp, error) {
	var out gen.OpenrailsPsp
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var environment string
		err := tx.QueryRow(ctx, `SELECT environment FROM openrails.psps WHERE id = $1 AND merchant_id = $2 AND rail = $3`,
			pspID, id.UUID(), rail).Scan(&environment)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPaymentProviderAccountNotFound
		}
		if err != nil {
			return err
		}
		rows, err := lockRailPSPs(ctx, tx, id, rail, environment)
		if err != nil {
			return err
		}
		var target *gen.OpenrailsPsp
		otherActive := 0
		for i := range rows {
			switch {
			case rows[i].ID == pspID:
				target = &rows[i]
			case !rows[i].Archived:
				otherActive++
			}
		}
		if target == nil {
			return ErrPaymentProviderAccountNotFound
		}
		if target.Archived {
			out = *target
			return nil
		}
		if otherActive == 0 && !allowLast {
			return &LastActiveProviderAccountError{
				Rail:        target.Rail,
				Environment: target.Environment,
				Account:     ProviderAccountRef{ID: target.ID, AccountID: target.AccountID},
			}
		}
		out, err = markPSPArchived(ctx, tx, id, target.ID)
		return err
	})
	return out, err
}

// archiveSoleActivePSP is the rail-level archive: exactly one active account
// on (rail, environment) is archived; none is ErrPaymentProviderNotFound and
// several is MultipleActiveProviderAccountsError.
func (s *Service) archiveSoleActivePSP(ctx context.Context, id merchant.ID, rail, environment string) (gen.OpenrailsPsp, error) {
	var out gen.OpenrailsPsp
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := lockRailPSPs(ctx, tx, id, rail, environment)
		if err != nil {
			return err
		}
		var active []gen.OpenrailsPsp
		for _, row := range rows {
			if !row.Archived {
				active = append(active, row)
			}
		}
		switch len(active) {
		case 0:
			return ErrPaymentProviderNotFound
		case 1:
			out, err = markPSPArchived(ctx, tx, id, active[0].ID)
			return err
		default:
			refs := make([]ProviderAccountRef, 0, len(active))
			for _, row := range active {
				refs = append(refs, ProviderAccountRef{ID: row.ID, AccountID: row.AccountID})
			}
			return &MultipleActiveProviderAccountsError{Rail: rail, Environment: environment, Accounts: refs}
		}
	})
	return out, err
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

func applyOpenObligations(c *PaymentProviderConfig, count int64) {
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
		if ref, published := evidence.CredentialRefs[NormalizeCredentialVersionKey(key)]; published {
			_, ok = configured[ref.Name]
		}
		if evidence.RetiredCredentials[NormalizeCredentialVersionKey(key)] {
			ok = false
		}
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
		Revision:       evidence.Revision,
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
		if ref, ok := CredentialRefs(row.Evidence)[NormalizeCredentialVersionKey(key)]; ok {
			name = ref.Name
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
// the PSP catalog.
func supportedPaymentProvider(provider string) bool {
	return rails.SupportsPSPs(models.Rail(provider))
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

func unmarshalProviderEvidence(raw []byte) pspEvidence {
	if len(raw) == 0 {
		return pspEvidence{}
	}
	var out pspEvidence
	_ = json.Unmarshal(raw, &out)
	return out
}

// refuseLiveNMIUnderTestMode requires a fresh simulated result before arming
// sandbox NMI credentials. Live or indeterminate responses refuse the arm.
func (s *Service) refuseLiveNMIUnderTestMode(ctx context.Context, id merchant.ID, rail, environment, accountID string, credentials map[string]string) error {
	if rail != string(models.RailNMI) {
		return nil
	}
	sandbox := s.providerEnvironment == "test"
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
		if sec, gerr := s.readPublishedProviderCredential(ctx, id, rail, environment, accountID, "security_key", name); gerr == nil {
			securityKey = strings.TrimSpace(sec.Value)
		} else if !errors.Is(gerr, ErrSecretNotFound) {
			return fmt.Errorf("read effective NMI credential for sandbox qualification: %w", gerr)
		}
	}
	if securityKey == "" {
		return nil // unconfigured; nothing to verify
	}
	deployment, pspID, err := s.storedNMIDeployment(ctx, id, rail, environment, accountID)
	if err != nil {
		return err
	}
	client, err := nmi.NewAccountClient(id.UUID(), pspID, accountID, &config.NMIProviderSettings{SecurityKey: securityKey, EndpointDeployment: deployment}, sandbox)
	if err != nil {
		return fmt.Errorf("construct NMI posture qualification client: %w", err)
	}
	if s.nmiProbeV5BaseURL != "" {
		client.V5BaseURL = s.nmiProbeV5BaseURL
	}
	check := nmi.CheckTestModeArm
	if !sandbox {
		// SEC-33: a live deployment refuses an NMI account left in test mode.
		check = nmi.CheckLiveArm
	}
	if err := check(ctx, client); err != nil {
		return providerCredentialError(fmt.Errorf("merchants: rail %q account %q: %w", rail, accountID, err))
	}
	return nil
}

// storedNMIDeployment reads the declared endpoint deployment and PSP id; an
// undeclared PSP uses the default deployment and its derived natural-key id.
func (s *Service) storedNMIDeployment(ctx context.Context, id merchant.ID, rail, environment, accountID string) (string, uuid.UUID, error) {
	pspID, _, _, _ := PSPNaturalKey(rail, environment, accountID)
	if s.pool == nil {
		return "", pspID, nil
	}
	var stored struct {
		Settings map[string]any `json:"settings"`
	}
	row, err := gen.New(s.pool).GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{MerchantID: id.UUID(), Rail: rail, Environment: &environment, AccountID: accountID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, err
	}
	if err == nil {
		pspID = row.ID
		if err := json.Unmarshal(row.Evidence, &stored); err != nil {
			return "", uuid.Nil, err
		}
	}
	deployment, err := config.NMIEndpointDeployment(stored.Settings)
	return deployment, pspID, err
}

// Read only the registry-bounded slots for this account. Published references
// select exact custody; fallback names are used only for unpublished slots.
func (s *Service) paymentProviderCredentialStatuses(ctx context.Context, id merchant.ID, row gen.OpenrailsPsp) ([]MerchantSecretStatus, error) {
	var statuses []MerchantSecretStatus
	for _, key := range paymentProviderCredentialKeys(row.Rail) {
		ref, err := PSPSecretRef(row.Rail, row.Environment, row.AccountID, row.Evidence, key)
		if err != nil {
			return nil, err
		}
		if ref.Retired {
			continue
		}
		value, err := ReadSecretRef(ctx, s.secrets, id, ref)
		if err != nil && !errors.Is(err, ErrSecretNotFound) {
			return nil, err
		}
		statuses = append(statuses, MerchantSecretStatus{Name: ref.Name, Key: key, Rail: row.Rail, Configured: err == nil, Version: value.Version})
	}
	return statuses, nil
}
