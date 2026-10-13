package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

// WithPlatformVaultStore uses the engine's authenticated Vault connection.
func (s *Service) WithPlatformVaultStore(kv *vault.KVv2Adapter) { s.platformKV = kv }

// PlatformCardCustodian selects custody for new cards only. Existing instruments
// and accepted attempts retain their own custodian when a merchant switches.
func (s *Service) PlatformCardCustodian(ctx context.Context, id billing.MerchantID) (*CustodianScope, error) {
	set, err := s.config.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if merchantconfig.CardCustody(set.Merchant.Value.Settings) != billing.CardCustodyPlatform {
		return nil, nil
	}
	c, ok, err := s.CustodianScopeByKey(ctx, id, config.PlatformCustodianKey)
	if err != nil {
		return nil, err
	}
	if !ok || c.Archived || c.Kind != models.CustodianPlatform {
		return nil, ErrPlatformVaultUnavailable
	}
	return &c, nil
}

// PlatformCustodianConfig reads operator-managed credentials, never a merchant
// document. Each merchant has its own public collection Application: token
// intents have no containers, so their vendor-reported created_by is the
// boundary checked before a charge. Durable tokens use the merchant container.
func (s *Service) PlatformCustodianConfig(ctx context.Context, id billing.MerchantID, custodianID uuid.UUID) (*config.ResolvedCustodian, error) {
	if s.platformVault == nil || s.platformKV == nil {
		return nil, ErrPlatformVaultUnavailable
	}
	c, ok, err := s.CustodianScopeByID(ctx, id, custodianID)
	if err != nil {
		return nil, err
	}
	if !ok || c.Archived || c.Kind != models.CustodianPlatform || c.AccountID != config.PlatformCustodianAccountID(s.platformVault, id.UUID()) {
		return nil, ErrPlatformVaultUnavailable
	}
	prefix := strings.Trim(s.platformPrefix, "/")
	if prefix == "" {
		prefix = merchantdocs.DefaultPrefix
	}
	doc, err := s.platformKV.ReadDocument(ctx, prefix+"/platform_vault/"+s.platformVault.TenantID)
	if err != nil {
		return nil, err
	}
	if !doc.Found {
		return nil, ErrPlatformVaultUnavailable
	}
	var keys struct {
		APIKey                 string `json:"api_key"`
		CollectionApplications map[string]struct {
			ID           string `json:"id"`
			PublicAPIKey string `json:"public_api_key"`
		} `json:"collection_applications"`
	}
	if err := json.Unmarshal(doc.Data, &keys); err != nil {
		return nil, errors.New("platform vault credential document is invalid")
	}
	app, ok := keys.CollectionApplications[id.UUID().String()]
	if !ok || strings.TrimSpace(keys.APIKey) == "" || strings.TrimSpace(app.PublicAPIKey) == "" || app.PublicAPIKey == keys.APIKey {
		return nil, ErrPlatformVaultUnavailable
	}
	applicationID, err := uuid.Parse(app.ID)
	if err != nil || applicationID == uuid.Nil {
		return nil, errors.New("platform vault collection application id is invalid")
	}
	for merchant, other := range keys.CollectionApplications {
		otherID, _ := uuid.Parse(other.ID)
		if merchant != id.UUID().String() && (otherID == applicationID || other.PublicAPIKey == app.PublicAPIKey) {
			return nil, errors.New("platform vault collection applications must be merchant-specific")
		}
	}
	return &config.ResolvedCustodian{Key: config.PlatformCustodianKey, Custodian: models.CustodianPlatform,
		AccountID: s.platformVault.TenantID, APIKey: keys.APIKey, PublicAPIKey: app.PublicAPIKey,
		CollectionApplicationID: applicationID.String(), Container: "/merchants/" + id.UUID().String() + "/"}, nil
}
