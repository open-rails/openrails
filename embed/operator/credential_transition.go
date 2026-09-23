package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type CredentialTransitionParams = merchants.CredentialTransitionRequest

// TransitionProviderCredentials moves one account's credential custody between
// explicitly constructed local runtimes. It preserves source material. A target
// snapshot must already contain all current and overlap credentials under its
// stable CredentialSnapshotID; every later restart must supply that snapshot.
// Neither backend connections nor this maintenance operation are exposed by a
// remote Client or HTTP route.
func (o *Operator) TransitionProviderCredentials(ctx context.Context, id merchant.ID, rail string, params CredentialTransitionParams, source *embed.Runtime) (*openrails.PaymentProviderConfig, error) {
	if err := o.initialized(); err != nil {
		return nil, err
	}
	from := New(source)
	if err := from.initialized(); err != nil {
		return nil, err
	}
	if id.IsZero() {
		return nil, fmt.Errorf("credential transition requires a merchant")
	}
	if config.ExpectedProviderEnvironment(o.app.Config.IsTestMode()) != config.ExpectedProviderEnvironment(from.app.Config.IsTestMode()) {
		return nil, fmt.Errorf("credential transition requires matching provider environments")
	}
	for _, selected := range []*Operator{o, from} {
		if bound := selected.app.Runtime.ConfiguredMerchant(); !bound.IsZero() && bound != id {
			return nil, fmt.Errorf("credential transition merchant differs from the runtime binding")
		}
		if selected.app.Runtime.Merchants == nil {
			return nil, fmt.Errorf("credential transition requires initialized merchant services")
		}
	}
	result, err := o.app.Runtime.Merchants.TransitionProviderCredentials(ctx, id, rail, params, from.app.Runtime.Merchants.Secrets())
	if err != nil {
		return nil, err
	}
	return &result, nil
}
