package railresolve

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// HyperSwitchClient resolves one exact merchant-owned custodian credential.
// The caller selects and locks the row, applies operation/archive policy, and
// qualifies the concrete deployment contract before granting SDK or PSP access.
// An archive does not itself revoke an existing obligation's custody.
func HyperSwitchClient(ctx context.Context, cfg *config.Config, secrets merchants.MerchantSecretReader, owner merchant.ID, row gen.OpenrailsCustodian) (*hyperswitch.Client, error) {
	if cfg == nil || cfg.HyperSwitch == nil || secrets == nil || owner.IsZero() || row.ID == uuid.Nil || row.MerchantID != owner.UUID() || row.Kind != models.CustodianHyperSwitch || row.Environment != config.ExpectedProviderEnvironment(cfg.IsTestMode()) {
		return nil, hyperswitch.ErrBinding
	}
	var settings map[string]any
	var versions map[string]*int
	if json.Unmarshal(row.Settings, &settings) != nil || settings == nil || json.Unmarshal(row.CredentialVersions, &versions) != nil || versions == nil {
		return nil, hyperswitch.ErrBinding
	}
	for _, version := range versions {
		if version == nil || *version < 0 {
			return nil, hyperswitch.ErrBinding
		}
	}
	floor := 0
	if version := versions[custodians.SecretAPIKey]; version != nil {
		floor = *version
	}
	parsed, err := custodians.ParseSettings(row.Kind, settings)
	if err != nil {
		return nil, hyperswitch.ErrBinding
	}
	name, err := merchants.CustodianSecretName(row.Kind, row.Environment, row.AccountID, custodians.SecretAPIKey)
	if err != nil {
		return nil, hyperswitch.ErrBinding
	}
	secret, err := merchants.ReadSecretRef(ctx, secrets, owner, merchants.SecretRef{Name: name, MinVersion: floor})
	if err != nil || strings.TrimSpace(secret.Value) == "" || secret.Version < floor {
		return nil, hyperswitch.ErrUnavailable
	}
	return hyperswitch.New(hyperswitch.Config{BaseURL: cfg.HyperSwitch.APIBaseURL, MerchantID: row.AccountID, ProfileID: parsed.ProfileID, APIKey: hyperswitch.Secret(secret.Value), ReadOnly: cfg.IsProviderReadOnly()})
}
