package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	hscharge "github.com/open-rails/openrails/internal/modules/payments/rails/hyperswitch"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

type hyperSwitchCollectionAdapter struct {
	charger *hscharge.Charger
	binding charge.HyperSwitchBinding
}

// SetHyperSwitchDeployment is runtime initialization, before serving or workers.
// The operator URL cannot come from merchant settings or change on a service.
func (s *MoneyService) SetHyperSwitchDeployment(apiBaseURL string) error {
	if s.hyperSwitchDeployment != "" {
		return errors.New("HyperSwitch deployment is already configured")
	}
	canonical, err := charge.CanonicalHyperSwitchDeployment(apiBaseURL)
	if err != nil {
		return err
	}
	s.hyperSwitchDeployment = canonical
	return nil
}

func collectionHyperSwitchBinding(ctx context.Context, q *gen.Queries, method gen.OpenrailsPaymentMethod, deployment string) (charge.HyperSwitchBinding, error) {
	var empty charge.HyperSwitchBinding
	if deployment == "" || method.Custodian != models.CustodianHyperSwitch || method.CustodianID == nil {
		return empty, fmt.Errorf("%w: HyperSwitch custody is not configured", charge.ErrInstrumentChanged)
	}
	accounts, err := q.GetCollectionCustodianAccountsForShare(ctx, gen.GetCollectionCustodianAccountsForShareParams{MerchantID: method.MerchantID, PspID: method.PspID, CustodianID: *method.CustodianID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, charge.ErrInstrumentChanged
	}
	if err != nil {
		return empty, err
	}
	row := accounts.OpenrailsCustodian
	if accounts.OpenrailsPsp.Rail != method.Rail || row.Kind != method.Custodian || row.Environment != accounts.OpenrailsPsp.Environment {
		return empty, charge.ErrInstrumentChanged
	}
	return hyperSwitchBinding(row, deployment)
}

func hyperSwitchBinding(row gen.OpenrailsCustodian, deployment string) (charge.HyperSwitchBinding, error) {
	var settings map[string]any
	if json.Unmarshal(row.Settings, &settings) != nil {
		return charge.HyperSwitchBinding{}, charge.ErrInstrumentChanged
	}
	parsed, err := custodians.ParseSettings(row.Kind, settings)
	if err != nil {
		return charge.HyperSwitchBinding{}, charge.ErrInstrumentChanged
	}
	canonical, err := charge.CanonicalHyperSwitchDeployment(deployment)
	if err != nil {
		return charge.HyperSwitchBinding{}, err
	}
	binding := charge.HyperSwitchBinding{AccountID: row.AccountID, ProfileID: parsed.ProfileID, APIBaseURL: canonical}
	return binding, binding.Validate()
}

func (a *hyperSwitchCollectionAdapter) Prepare(ctx context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (PreparedCharge, error) {
	if req.HyperSwitch == nil || *req.HyperSwitch != a.binding {
		return nil, fmt.Errorf("%w: accepted HyperSwitch custody profile changed", charge.ErrInstrumentChanged)
	}
	if method.Custodian != models.CustodianHyperSwitch || method.CustodianID == nil || method.ChargeVia != "pan_proxy" || method.RailCustomerRef == "" || method.RailMethodRef == "" || method.ParkReason != "" {
		return nil, fmt.Errorf("HyperSwitch collection requires an owned, usable permanent card")
	}
	// Read-only contract qualification precedes the durable submission marker.
	// Charge repeats it immediately before sending; loss then stays uncertain.
	if err := a.charger.Client.CheckProxyContract(ctx, a.charger.Destination); err != nil {
		return nil, err
	}
	return prepareUnscheduledCollection(method, req, a.charger)
}

func (b *MerchantCollectionAdapterBuilder) hyperSwitchAdapter(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (CollectionAdapter, error) {
	if b.Config == nil || b.Config.HyperSwitch == nil || scope.CustodianID == nil || b.Config.IsProviderReadOnly() {
		return nil, fmt.Errorf("HyperSwitch invoice collection is not armed")
	}
	custodian, err := b.DB.Gen(ctx).GetCustodian(ctx, *scope.CustodianID)
	if err != nil {
		return nil, err
	}
	if custodian.MerchantID != mid.UUID() || custodian.Kind != models.CustodianHyperSwitch || custodian.Environment != scope.Environment || scope.Environment != config.ExpectedProviderEnvironment(b.testMode()) {
		return nil, fmt.Errorf("HyperSwitch invoice custody does not match the accepted account")
	}
	gatewayKey, err := b.requireSecret(ctx, svc, mid, scope, "security_key")
	if err != nil {
		return nil, err
	}
	client, err := railresolve.HyperSwitchClient(ctx, b.Config, svc.Secrets(), mid, custodian)
	if err != nil {
		return nil, err
	}
	binding, err := hyperSwitchBinding(custodian, b.Config.HyperSwitch.APIBaseURL)
	if err != nil {
		return nil, err
	}
	destination := strings.TrimSpace(b.Endpoints.NMIDirectPostURL)
	if destination == "" {
		destination = nmi.DefaultDirectPostURL
		if b.testMode() {
			destination = nmi.SandboxDirectPostURL
		}
	}
	return &hyperSwitchCollectionAdapter{binding: binding, charger: &hscharge.Charger{Client: client, Destination: destination, SecurityKey: hyperswitch.Secret(gatewayKey)}}, nil
}
