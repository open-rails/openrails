package railresolve

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/merchants"
)

// HyperSwitchClient builds the client of one merchant-owned HyperSwitch
// custodian from its configuration. The caller selects the custodian, applies
// operation/archive policy, and qualifies the concrete deployment contract
// before granting SDK or PSP access. An archive does not itself revoke an
// existing obligation's custody.
func HyperSwitchClient(cfg *config.Config, owner billing.MerchantID, custodian merchants.CustodianScope) (*hyperswitch.Client, error) {
	if cfg == nil || cfg.HyperSwitch == nil || owner.IsZero() || custodian.ID == uuid.Nil || custodian.Kind != models.CustodianHyperSwitch || custodian.Environment != config.ExpectedProviderEnvironment(config.IsTestMode(cfg)) {
		return nil, hyperswitch.ErrBinding
	}
	parsed, err := custodians.ParseSettings(custodian.Kind, custodian.Settings)
	if err != nil {
		return nil, hyperswitch.ErrBinding
	}
	apiKey := custodian.Secret(custodians.SecretAPIKey)
	if apiKey == "" {
		return nil, hyperswitch.ErrUnavailable
	}
	return hyperswitch.New(hyperswitch.Config{BaseURL: cfg.HyperSwitch.APIBaseURL, MerchantID: custodian.AccountID, ProfileID: parsed.ProfileID, APIKey: hyperswitch.Secret(apiKey), ReadOnly: config.IsProviderReadOnly(cfg)})
}
