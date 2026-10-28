package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	log "github.com/sirupsen/logrus"
)

// WithPlatformVaultStore uses the engine's authenticated Vault connection.
func (s *Service) WithPlatformVaultStore(kv *vault.KVv2Adapter) { s.platformKV = kv }

// BasisTheoryCustodianConfig resolves the saved instrument's original custody.
// Merchant credentials never fall back to the operator's hosted vault.
func (s *Service) BasisTheoryCustodianConfig(ctx context.Context, id billing.MerchantID, custodianID uuid.UUID) (*config.ResolvedCustodian, error) {
	c, ok, err := s.CustodianScopeByID(ctx, id, custodianID)
	if err != nil {
		return nil, err
	}
	if !ok || c.Archived {
		return nil, ErrCustodianNotDeclared
	}
	if c.Kind == models.CustodianPlatform {
		return s.PlatformCustodianConfig(ctx, id, custodianID)
	}
	if c.Kind != models.CustodianBasisTheory || c.Secret(custodians.SecretAPIKey) == "" {
		return nil, ErrCustodianNotDeclared
	}
	settings, err := custodians.ParseSettings(c.Kind, c.Settings)
	if err != nil {
		return nil, err
	}
	base := basistheory.DefaultBaseURL
	if c.Environment == config.ClassTest {
		base = "https://api.test.basistheory.com"
	}
	return &config.ResolvedCustodian{Key: c.Key, Custodian: c.Kind, Environment: c.Environment, AccountID: c.AccountID, APIKey: c.Secret(custodians.SecretAPIKey), PublicAPIKey: settings.PublicAPIKey, APIBaseURL: base,
		AccountUpdater: settings.AccountUpdater, AccountUpdaterLookaheadDays: settings.LookaheadDays()}, nil
}

// PlatformCardCustodian is the platform vault holding new cards when the
// merchant's card_vault is platform; nil otherwise. Existing instruments and
// accepted attempts retain their own custodian when a merchant switches.
func (s *Service) PlatformCardCustodian(ctx context.Context, id billing.MerchantID) (*CustodianScope, error) {
	set, err := s.config.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !merchantconfig.PlatformCardVault(set.Merchant.Value.MerchantSettings) {
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
	base := basistheory.DefaultBaseURL
	if c.Environment == config.ClassTest {
		base = "https://api.test.basistheory.com"
	}
	armed, lookahead := platformAccountUpdater(ctx, doc.Data)
	return &config.ResolvedCustodian{Key: config.PlatformCustodianKey, Custodian: models.CustodianPlatform,
		Environment: c.Environment,
		APIBaseURL:  base,
		AccountID:   s.platformVault.TenantID, APIKey: keys.APIKey, PublicAPIKey: app.PublicAPIKey,
		CollectionApplicationID: applicationID.String(), Container: "/merchants/" + id.UUID().String() + "/",
		AccountUpdater: armed, AccountUpdaterLookaheadDays: lookahead}, nil
}

// platformAccountUpdater reads the Account Updater settings of the platform
// document apart from its credentials: a malformed optional value leaves the
// updater unarmed and every other use of the vault working.
func platformAccountUpdater(ctx context.Context, doc []byte) (bool, int) {
	var raw struct {
		Armed     json.RawMessage `json:"account_updater"`
		Lookahead json.RawMessage `json:"account_updater_lookahead_days"`
	}
	var armed bool
	lookahead := 0
	if json.Unmarshal(doc, &raw) != nil || len(raw.Armed) == 0 || json.Unmarshal(raw.Armed, &armed) != nil {
		if len(raw.Armed) != 0 {
			log.WithContext(ctx).Warn("platform vault: account_updater is not a boolean; the Account Updater stays unarmed")
		}
		return false, custodians.DefaultAccountUpdaterLookaheadDays
	}
	if len(raw.Lookahead) != 0 && (json.Unmarshal(raw.Lookahead, &lookahead) != nil || lookahead < 1 || lookahead > custodians.MaxAccountUpdaterLookaheadDays) {
		log.WithContext(ctx).Warnf("platform vault: account_updater_lookahead_days must be a whole number from 1 to %d; the Account Updater stays unarmed", custodians.MaxAccountUpdaterLookaheadDays)
		return false, custodians.DefaultAccountUpdaterLookaheadDays
	}
	return armed, custodians.Settings{AccountUpdaterLookaheadDays: lookahead}.LookaheadDays()
}
