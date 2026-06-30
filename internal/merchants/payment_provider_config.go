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

	"github.com/open-rails/openrails/internal/db/gen"
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
	ID             uuid.UUID                                  `json:"id"`
	ProviderType   string                                     `json:"provider_type"`
	Environment    string                                     `json:"environment"`
	AccountID      string                                     `json:"account_id"`
	Routing        string                                     `json:"routing"`
	Status         string                                     `json:"status"`
	PublicConfig   map[string]string                          `json:"public_config,omitempty"`
	Credentials    map[string]PaymentProviderCredentialStatus `json:"credentials"`
	FirstSeenAt    time.Time                                  `json:"first_seen_at"`
	LastVerifiedAt *time.Time                                 `json:"last_validated_at,omitempty"`
	ReplacedAt     *time.Time                                 `json:"replaced_at,omitempty"`
	CreatedAt      time.Time                                  `json:"created_at"`
	UpdatedAt      time.Time                                  `json:"updated_at"`
}

// UpsertPaymentProviderConfigRequest creates or replaces one provider account.
type UpsertPaymentProviderConfigRequest struct {
	Environment  string            `json:"environment"`
	Enabled      *bool             `json:"enabled"`
	AccountID    string            `json:"account_id"`
	PublicConfig map[string]string `json:"public_config"`
	Credentials  map[string]string `json:"credentials"`
}

type providerAccountEvidence struct {
	PublicConfig map[string]string `json:"public_config,omitempty"`
}

// ListPaymentProviderConfigs returns provider-account configs for a merchant.
func (s *Service) ListPaymentProviderConfigs(ctx context.Context, id merchant.ID, provider, environment, status string) ([]PaymentProviderConfig, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("merchants: pgx pool is required")
	}
	provider = normalizeProviderSecretType(provider)
	environment = normalizeProviderSecretEnvironment(environment)
	status = strings.ToLower(strings.TrimSpace(status))

	var rows []gen.OpenrailsProviderAccount
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		var providerFilter *string
		if provider != "" {
			providerFilter = &provider
		}
		var err error
		rows, err = gen.New(tx).ListProviderAccountsForMerchant(ctx, gen.ListProviderAccountsForMerchantParams{
			MerchantID:   id.UUID(),
			ProviderType: providerFilter,
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
	out := make([]PaymentProviderConfig, 0, len(rows))
	for _, row := range rows {
		if environment != "" && row.Environment != environment {
			continue
		}
		if status != "" && row.Status != status {
			continue
		}
		out = append(out, paymentProviderConfigFromRow(row, statuses))
	}
	return out, nil
}

// GetPaymentProviderConfig returns the enabled primary config for provider/env.
func (s *Service) GetPaymentProviderConfig(ctx context.Context, id merchant.ID, provider, environment string) (PaymentProviderConfig, error) {
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		environment = "live"
	}
	items, err := s.ListPaymentProviderConfigs(ctx, id, provider, environment, "enabled")
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	for _, item := range items {
		if item.Routing == "primary" {
			return item, nil
		}
	}
	return PaymentProviderConfig{}, ErrSecretNotFound
}

// UpsertPaymentProviderConfig validates credentials first, then stores the
// provider account and scoped secrets.
func (s *Service) UpsertPaymentProviderConfig(ctx context.Context, id merchant.ID, provider string, req UpsertPaymentProviderConfigRequest) (PaymentProviderConfig, error) {
	if s == nil || s.pool == nil || s.secrets == nil {
		return PaymentProviderConfig{}, errors.New("merchants: provider config storage unavailable")
	}
	provider = normalizeProviderSecretType(provider)
	if !supportedPaymentProvider(provider) {
		return PaymentProviderConfig{}, fmt.Errorf("merchants: unsupported payment provider %q", provider)
	}
	environment := normalizeProviderSecretEnvironment(req.Environment)
	if environment == "" {
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
		name, err := ProviderAccountSecretName(provider, environment, accountID, key)
		if err != nil {
			return PaymentProviderConfig{}, err
		}
		if err := s.ValidateCredential(ctx, id, name, value, nil); err != nil {
			return PaymentProviderConfig{}, err
		}
		secretNames[name] = value
	}

	row, err := s.upsertPaymentProviderAccount(ctx, id, provider, environment, accountID, enabled, req.PublicConfig)
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
	return paymentProviderConfigFromRow(row, statuses), nil
}

// DeletePaymentProviderConfig disables one provider environment and removes its
// scoped credentials. Historical provider_accounts rows remain for audit links.
func (s *Service) DeletePaymentProviderConfig(ctx context.Context, id merchant.ID, provider, environment string) (PaymentProviderConfig, error) {
	if s == nil || s.pool == nil || s.secrets == nil {
		return PaymentProviderConfig{}, errors.New("merchants: provider config storage unavailable")
	}
	current, err := s.GetPaymentProviderConfig(ctx, id, provider, environment)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	row, err := s.disablePaymentProviderAccount(ctx, id, current.ID)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	for _, key := range paymentProviderCredentialKeys(row.ProviderType) {
		name, err := ProviderAccountSecretName(row.ProviderType, row.Environment, row.AccountID, key)
		if err == nil {
			_ = s.secrets.Delete(ctx, id, name)
		}
	}
	statuses, err := s.ListSecretStatuses(ctx, id)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	return paymentProviderConfigFromRow(row, statuses), nil
}

func (s *Service) upsertPaymentProviderAccount(ctx context.Context, id merchant.ID, provider, environment, accountID string, enabled bool, publicConfig map[string]string) (gen.OpenrailsProviderAccount, error) {
	evidence, err := marshalProviderEvidence(publicConfig)
	if err != nil {
		return gen.OpenrailsProviderAccount{}, err
	}
	status := "disabled"
	routing := "legacy"
	if enabled {
		status = "enabled"
		routing = "secondary"
	}
	var row gen.OpenrailsProviderAccount
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
				INSERT INTO openrails.provider_accounts (
				    merchant_id, provider_type, environment, account_id, routing, status, evidence, last_verified_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, now())
				ON CONFLICT (merchant_id, provider_type, environment, account_id) DO UPDATE SET
				    routing = EXCLUDED.routing,
				    status = EXCLUDED.status,
				    evidence = EXCLUDED.evidence,
				    last_verified_at = EXCLUDED.last_verified_at,
				    replaced_at = CASE WHEN EXCLUDED.status = 'disabled' THEN COALESCE(openrails.provider_accounts.replaced_at, now()) ELSE NULL END,
				    updated_at = now()
				RETURNING id, merchant_id, provider_type, environment, account_id, display_name, vault_secret_ref, routing, status, evidence, first_seen_at, last_verified_at, replaced_at, created_at, updated_at
			`, id.UUID(), provider, environment, accountID, routing, status, evidence).Scan(
			&row.ID, &row.MerchantID, &row.ProviderType, &row.Environment, &row.AccountID,
			&row.DisplayName, &row.VaultSecretRef, &row.Routing, &row.Status, &row.Evidence,
			&row.FirstSeenAt, &row.LastVerifiedAt, &row.ReplacedAt, &row.CreatedAt, &row.UpdatedAt,
		)
		if err != nil {
			return err
		}
		if !enabled {
			return nil
		}
		q := gen.New(tx)
		if err := q.DemoteOtherPrimaryProviderAccounts(ctx, gen.DemoteOtherPrimaryProviderAccountsParams{
			MerchantID:   id.UUID(),
			ProviderType: provider,
			Environment:  &environment,
			ID:           row.ID,
		}); err != nil {
			return err
		}
		row, err = q.PromoteProviderAccountToPrimary(ctx, gen.PromoteProviderAccountToPrimaryParams{
			ID:           row.ID,
			MerchantID:   id.UUID(),
			ProviderType: provider,
			Environment:  &environment,
		})
		return err
	})
	return row, err
}

func (s *Service) disablePaymentProviderAccount(ctx context.Context, id merchant.ID, accountID uuid.UUID) (gen.OpenrailsProviderAccount, error) {
	var row gen.OpenrailsProviderAccount
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
				UPDATE openrails.provider_accounts
				   SET status = 'disabled',
				       routing = 'legacy',
				       replaced_at = COALESCE(replaced_at, now()),
				       updated_at = now()
				 WHERE id = $1 AND merchant_id = $2
				RETURNING id, merchant_id, provider_type, environment, account_id, display_name, vault_secret_ref, routing, status, evidence, first_seen_at, last_verified_at, replaced_at, created_at, updated_at
			`, accountID, id.UUID()).Scan(
			&row.ID, &row.MerchantID, &row.ProviderType, &row.Environment, &row.AccountID,
			&row.DisplayName, &row.VaultSecretRef, &row.Routing, &row.Status, &row.Evidence,
			&row.FirstSeenAt, &row.LastVerifiedAt, &row.ReplacedAt, &row.CreatedAt, &row.UpdatedAt,
		)
	})
	return row, err
}

func paymentProviderConfigFromRow(row gen.OpenrailsProviderAccount, statuses []MerchantSecretStatus) PaymentProviderConfig {
	configured := map[string]struct{}{}
	for _, st := range statuses {
		if st.Configured {
			configured[cleanSecretName(st.Name)] = struct{}{}
		}
	}
	credentials := make(map[string]PaymentProviderCredentialStatus)
	for _, key := range paymentProviderCredentialKeys(row.ProviderType) {
		name, err := ProviderAccountSecretName(row.ProviderType, row.Environment, row.AccountID, key)
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
		ProviderType:   row.ProviderType,
		Environment:    row.Environment,
		AccountID:      row.AccountID,
		Routing:        row.Routing,
		Status:         row.Status,
		PublicConfig:   unmarshalProviderEvidence(row.Evidence).PublicConfig,
		Credentials:    credentials,
		FirstSeenAt:    row.FirstSeenAt,
		LastVerifiedAt: row.LastVerifiedAt,
		ReplacedAt:     row.ReplacedAt,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}
}

func supportedPaymentProvider(provider string) bool {
	switch normalizeProviderSecretType(provider) {
	case "stripe", "nmi", "ccbill", "solana":
		return true
	default:
		return false
	}
}

func paymentProviderCredentialKeys(provider string) []string {
	switch normalizeProviderSecretType(provider) {
	case "stripe":
		return []string{"secret_key", "webhook_signing_secret", "webhook_signing_secret_thin"}
	case "nmi":
		return []string{"production_key", "tokenization_key", "tokenization_url", "webhook_signing_secret"}
	case "ccbill":
		return []string{"account_config"}
	default:
		return nil
	}
}

func marshalProviderEvidence(publicConfig map[string]string) ([]byte, error) {
	if len(publicConfig) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(providerAccountEvidence{PublicConfig: publicConfig})
	if err != nil {
		return nil, fmt.Errorf("marshal provider config evidence: %w", err)
	}
	return b, nil
}

func unmarshalProviderEvidence(raw []byte) providerAccountEvidence {
	if len(raw) == 0 {
		return providerAccountEvidence{}
	}
	var out providerAccountEvidence
	_ = json.Unmarshal(raw, &out)
	return out
}
