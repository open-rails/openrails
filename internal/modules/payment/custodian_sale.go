package payment

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Custodian-held cards: the custodian (Basis Theory) holds the card and the
// NMI PSP's own gateway charges it through the custodian's proxy. Order pay,
// the NMI sale and platform card setup charge through this service.
// Network-token provisioning rides behind the merchant flag and is never
// load-bearing.

// CustodianSaleService charges custodian-held cards.
type CustodianSaleService struct {
	BasisTheoryTransport http.RoundTripper
	NMIClients           *railresolve.NMIFactory
	DB                   *db.DB
	Fulfillment          *FulfillmentService
	Rails                railresolve.Source
	Config               *config.Config
	// BTBaseURLOverride points every BT byte at a fake server (test seam).
	BTBaseURLOverride string
	// GatewayDirectPostURLOverride overrides the proxy destination (test seam).
	GatewayDirectPostURLOverride string
}

// custodialPSP is the resolved arrangement one custodian sale charges through:
// the PSP's own gateway credentials plus the custodian that holds the card.
type custodialPSP struct {
	Custody            *config.ResolvedCustodian
	GatewaySecurityKey string
	// The charging PSP's exact identity and declared credential set.
	MerchantID billing.MerchantID
	Scope      merchants.PSPScope
	Settings   *config.NMIProviderSettings
}

// resolveConfig arms the ctx merchant's custodian-held-card credentials: the
// NMI PSP that charges, and the custodian it references.
func (s *CustodianSaleService) resolveConfig(ctx context.Context) (*custodialPSP, error) {
	if s.Rails == nil {
		return nil, errors.New("rail resolution not configured")
	}
	rc, err := s.Rails.RailConfig(ctx, string(models.RailNMI), "")
	if err != nil {
		return nil, err
	}
	if rc.Custody == nil || rc.Custody.Custodian == "" || rc.Custody.Custodian == models.CustodianPSP {
		return nil, errors.New("nmi psp declares no third-party custodian; custodian purchase is not armed")
	}
	if rc.NMI == nil || strings.TrimSpace(rc.NMI.SecurityKey) == "" {
		return nil, errors.New("nmi psp resolved without a gateway security key")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return &custodialPSP{
		Custody:            rc.Custody,
		GatewaySecurityKey: rc.NMI.SecurityKey,
		MerchantID:         mid,
		Scope:              merchants.PSPScope{ID: rc.ID, Rail: string(models.RailNMI), AccountID: rc.EffectiveAccountID(), Key: rc.Key},
		Settings:           rc.ToNMIProviderSettings(),
	}, nil
}

// nmiFactory is the single PSP-scoped factory; the proxy destination seam is
// a loopback fixture like any other endpoint override.
func (s *CustodianSaleService) nmiFactory() *railresolve.NMIFactory {
	if s.NMIClients != nil {
		return s.NMIClients
	}
	return &railresolve.NMIFactory{Config: s.Config, Endpoints: railresolve.NMIEndpoints{DirectPostURL: s.GatewayDirectPostURLOverride}}
}

func (s *CustodianSaleService) btClient(cfg *custodialPSP) (*basistheory.Client, error) {
	if cfg == nil || cfg.Custody == nil || (cfg.Custody.Custodian != models.CustodianBasisTheory && cfg.Custody.Custodian != models.CustodianPlatform) {
		return nil, errors.New("this custodian does not support the Basis Theory sale transport")
	}
	if s.BasisTheoryTransport != nil && cfg.Custody.Environment != config.ClassTest {
		return nil, errors.New("live custody cannot use a test wire")
	}
	baseURL := s.BTBaseURLOverride
	if baseURL == "" {
		baseURL = cfg.Custody.APIBaseURL
	}
	return basistheory.New(basistheory.Config{
		APIKey:        cfg.Custody.APIKey,
		BaseURL:       baseURL,
		WebhookKeyURL: cfg.Custody.WebhookKeyURL,
		ReadOnly:      s.Config != nil && config.IsProviderReadOnly(s.Config),
		Transport:     s.BasisTheoryTransport,
	})
}

func (s *CustodianSaleService) charger(cfg *custodialPSP) (*nmiproxy.Charger, error) {
	bt, err := s.btClient(cfg)
	if err != nil {
		return nil, err
	}
	posture, err := s.nmiFactory().ProxyPosture(cfg.MerchantID, cfg.Scope, cfg.Settings)
	if err != nil {
		return nil, err
	}
	return nmiproxy.New(bt, nmiproxy.GatewayConfig{SecurityKey: cfg.GatewaySecurityKey, DirectPostURL: posture.DirectPostURL, Posture: posture}), nil
}

// basisTheory is the Basis Theory custody of the NMI PSP psp: its proxy
// charger and the custodian's API.
func (s *CustodianSaleService) basisTheory(ctx context.Context, psp uuid.UUID) (*custodialPSP, *nmiproxy.Charger, *basistheory.Client, error) {
	if s == nil {
		return nil, nil, nil, errors.New("custodian purchase is not configured")
	}
	cfg, err := s.resolveConfig(db.WithPSPID(ctx, psp))
	if err != nil {
		return nil, nil, nil, err
	}
	if cfg.Scope.ID != psp {
		return nil, nil, nil, errors.New("the custodian's PSP resolved as another account")
	}
	bt, err := s.btClient(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	proxy, err := s.charger(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, proxy, bt, nil
}

// provisionNetworkToken creates (or dedups onto) the card's network token and
// persists id/status/PAR. Failures warn — NT is never load-bearing on NMI.
func (s *CustodianSaleService) provisionNetworkToken(ctx context.Context, bt *basistheory.Client, merchantID, instrumentID uuid.UUID, tokenID, orderID string) {
	nt, err := bt.CreateNetworkToken(ctx, basistheory.NetworkTokenRequest{
		TokenID:        tokenID,
		IdempotencyKey: "btnt:" + orderID,
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("bt_token_id", tokenID).
			Warn("custodian sale: network token provisioning failed; instrument stays pan_proxy (never load-bearing)")
		return
	}
	if s.DB == nil {
		return
	}
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := paymentmethods.ApplyCardLifecycle(ctx, s.DB.NewWithPgxTx(tx).Gen(ctx), paymentmethods.CardEvent{
			MerchantID: merchantID, PaymentMethodID: instrumentID, Source: paymentmethods.SourceNetworkToken, EventRef: "provision:" + orderID,
			Card: paymentmethods.Card{NetworkTokenID: nt.ID, NetworkTokenStatus: nt.Status, NetworkTokenPAR: nt.PAR}, At: s.Fulfillment.now(),
		})
		return err
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("custodian sale: failed to persist network token on instrument")
	}
}

// validatePlatformCollection trusts vendor-reported identity, never browser
// metadata. A shared tenant alone cannot establish which merchant collected it.
func validatePlatformCollection(c *config.ResolvedCustodian, intent *basistheory.TokenIntent) error {
	if c.Custodian == models.CustodianBasisTheory {
		if intent == nil || intent.Type != "card" || intent.TenantID != c.AccountID {
			return apperr.Invalidf("payment.token was not collected in this merchant's vault").WithParam("payment.token")
		}
		return nil
	}
	if c.Custodian != models.CustodianPlatform {
		return nil
	}
	if intent == nil || intent.Type != "card" || intent.TenantID != c.AccountID || intent.CreatedBy != c.CollectionApplicationID {
		return apperr.Invalidf("payment.token was not collected by this merchant's platform vault application").WithParam("payment.token")
	}
	return nil
}
