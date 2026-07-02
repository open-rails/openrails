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

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PaymentProviderCredentialStatus is the redacted credential view returned to
// merchant admins. It never contains plaintext.
type PaymentProviderCredentialStatus struct {
	Configured      bool       `json:"configured"`
	LastValidatedAt *time.Time `json:"last_validated_at,omitempty"`
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

// UpsertPaymentProviderConfigRequest creates or replaces one provider account.
type UpsertPaymentProviderConfigRequest struct {
	Environment  string            `json:"environment"`
	Enabled      *bool             `json:"enabled"`
	AccountID    string            `json:"account_id"`
	PublicConfig map[string]string `json:"public_config"`
	Credentials  map[string]string `json:"credentials"`
}

type railMerchantAccountEvidence struct {
	PublicConfig map[string]string `json:"public_config,omitempty"`
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

	var rows []gen.OpenrailsRailMerchantAccount
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var providerFilter *string
		if rail != "" {
			providerFilter = &rail
		}
		var err error
		rows, err = gen.New(tx).ListRailMerchantAccountsForMerchant(ctx, gen.ListRailMerchantAccountsForMerchantParams{
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
	openObligations, err := s.railMerchantAccountOpenObligations(ctx, id, accountIDs)
	if err != nil {
		return nil, err
	}
	out := make([]PaymentProviderConfig, 0, len(rows))
	for _, row := range rows {
		if environment != "" && row.Environment != environment {
			continue
		}
		if status != "" && !railMerchantAccountLifecycleMatches(row.Archived, status) {
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
	items, err := s.ListPaymentProviderConfigs(ctx, id, rail, environment, "active")
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
	environment := strings.TrimSpace(req.Environment)
	if environment == "" {
		environment = s.providerEnvironment // deployment posture (#681)
	} else if environment = normalizeProviderSecretEnvironment(environment); environment == "" {
		return PaymentProviderConfig{}, fmt.Errorf("merchants: provider environment must be live or test")
	}
	accountID := strings.TrimSpace(req.AccountID)
	if accountID == "" {
		return PaymentProviderConfig{}, fmt.Errorf("merchants: provider account_id required")
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	secretNames := make(map[string]string, len(req.Credentials))
	for key, value := range req.Credentials {
		name, err := RailMerchantAccountSecretName(rail, environment, accountID, key)
		if err != nil {
			return PaymentProviderConfig{}, err
		}
		if err := s.ValidateCredential(ctx, id, name, value, nil); err != nil {
			return PaymentProviderConfig{}, err
		}
		secretNames[name] = value
	}

	row, err := s.upsertRailMerchantAccount(ctx, id, rail, environment, accountID, enabled, req.PublicConfig)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	for name, value := range secretNames {
		if _, err := s.PutCredential(ctx, id, name, value); err != nil {
			return PaymentProviderConfig{}, err
		}
	}
	statuses, err := s.ListSecretStatuses(ctx, id)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	cfg := paymentProviderConfigFromRow(row, statuses)
	openObligations, err := s.railMerchantAccountOpenObligations(ctx, id, []uuid.UUID{row.ID})
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
	openObligations, err := s.railMerchantAccountOpenObligations(ctx, id, []uuid.UUID{row.ID})
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	cfg.applyOpenObligations(openObligations[row.ID])
	return cfg, nil
}

func (s *Service) upsertRailMerchantAccount(ctx context.Context, id merchant.ID, rail, environment, accountID string, enabled bool, publicConfig map[string]string) (gen.OpenrailsRailMerchantAccount, error) {
	evidence, err := marshalProviderEvidence(publicConfig)
	if err != nil {
		return gen.OpenrailsRailMerchantAccount{}, err
	}
	// #650: reject a cross-merchant claim with a clear error before the upsert
	// (which would otherwise fail with an opaque unique-violation under RLS).
	if err := AssertRailMerchantAccountUnowned(ctx, gen.New(s.pool), id.UUID(), rail, environment, accountID); err != nil {
		return gen.OpenrailsRailMerchantAccount{}, err
	}
	archived := !enabled
	var row gen.OpenrailsRailMerchantAccount
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = gen.New(tx).UpsertRailMerchantAccount(ctx, gen.UpsertRailMerchantAccountParams{
			MerchantID:  id.UUID(),
			Rail:        rail,
			Environment: &environment,
			AccountID:   accountID,
			Archived:    &archived,
			Evidence:    evidence,
		})
		return err
	})
	return row, err
}

func (s *Service) disableRailMerchantAccount(ctx context.Context, id merchant.ID, accountID uuid.UUID) (gen.OpenrailsRailMerchantAccount, error) {
	var row gen.OpenrailsRailMerchantAccount
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
				UPDATE openrails.rail_merchant_accounts
				   SET archived = true,
				       replaced_at = COALESCE(replaced_at, now()),
				       updated_at = now()
				 WHERE id = $1 AND merchant_id = $2
				RETURNING id, merchant_id, rail, environment, account_id, display_name, evidence, first_seen_at, last_verified_at, replaced_at, created_at, updated_at, owner, archived
			`, accountID, id.UUID()).Scan(
			&row.ID, &row.MerchantID, &row.Rail, &row.Environment, &row.AccountID,
			&row.DisplayName, &row.Evidence,
			&row.FirstSeenAt, &row.LastVerifiedAt, &row.ReplacedAt, &row.CreatedAt, &row.UpdatedAt,
			&row.Owner, &row.Archived,
		)
	})
	return row, err
}

func (s *Service) railMerchantAccountOpenObligations(ctx context.Context, id merchant.ID, accountIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
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
			            AND sub.rail_merchant_account_id = target.id
			            AND sub.status IN ('active', 'pending', 'past_due')
			       ) +
			       (
			         SELECT count(*)::bigint
			           FROM openrails.payments payment
			          WHERE payment.merchant_id = $2::uuid
			            AND payment.rail_merchant_account_id = target.id
			            AND payment.status = 'pending'
			       ) +
			       (
			         SELECT count(*)::bigint
			           FROM openrails.rail_intents intent
			          WHERE intent.merchant_id = $2::uuid
			            AND intent.rail_merchant_account_id = target.id
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

func paymentProviderConfigFromRow(row gen.OpenrailsRailMerchantAccount, statuses []MerchantSecretStatus) PaymentProviderConfig {
	configured := map[string]struct{}{}
	for _, st := range statuses {
		if st.Configured {
			configured[cleanSecretName(st.Name)] = struct{}{}
		}
	}
	credentials := make(map[string]PaymentProviderCredentialStatus)
	for _, key := range paymentProviderCredentialKeys(row.Rail) {
		name, err := RailMerchantAccountSecretName(row.Rail, row.Environment, row.AccountID, key)
		if err != nil {
			continue
		}
		_, ok := configured[name]
		credentials[key] = PaymentProviderCredentialStatus{
			Configured:      ok,
			LastValidatedAt: row.LastVerifiedAt,
		}
	}
	return PaymentProviderConfig{
		ID:             row.ID,
		Rail:           row.Rail,
		Environment:    row.Environment,
		AccountID:      row.AccountID,
		Archived:       row.Archived,
		PublicConfig:   unmarshalProviderEvidence(row.Evidence).PublicConfig,
		Credentials:    credentials,
		FirstSeenAt:    row.FirstSeenAt,
		LastVerifiedAt: row.LastVerifiedAt,
		ReplacedAt:     row.ReplacedAt,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func railMerchantAccountLifecycleMatches(archived bool, status string) bool {
	switch status {
	case "active", "enabled", "available", "not_archived":
		return !archived
	case "archived", "disabled", "legacy":
		return archived
	default:
		return false
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

func marshalProviderEvidence(publicConfig map[string]string) ([]byte, error) {
	if len(publicConfig) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(railMerchantAccountEvidence{PublicConfig: publicConfig})
	if err != nil {
		return nil, fmt.Errorf("marshal provider config evidence: %w", err)
	}
	return b, nil
}

func unmarshalProviderEvidence(raw []byte) railMerchantAccountEvidence {
	if len(raw) == 0 {
		return railMerchantAccountEvidence{}
	}
	var out railMerchantAccountEvidence
	_ = json.Unmarshal(raw, &out)
	return out
}
