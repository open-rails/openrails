package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
)

// StripeEngineSetupParams binds a nonfinancial card setup to an owned checkout
// resource. A successful SetupIntent alone never creates a paid membership.
type StripeEngineSetupParams struct {
	MerchantID, PSPID, CustomerID, SessionID uuid.UUID
	CustomerRef                              string
}
type StripeEngineSetup struct {
	ID                string
	Status            string
	ClientSecret      string `json:"-"` // ephemeral; never persist this struct
	MethodRef         string
	LastFour          string
	Brand             string
	ExpMonth, ExpYear int
}

func (p StripeEngineSetupParams) metadata() map[string]string {
	return map[string]string{"openrails_setup_session": p.SessionID.String(), "openrails_merchant": p.MerchantID.String(), "openrails_psp": p.PSPID.String(), "openrails_customer": p.CustomerID.String()}
}
func (s *StripeService) checkEngineSetup(p StripeEngineSetupParams) error {
	if s == nil || s.Config == nil || p.MerchantID == uuid.Nil || p.PSPID == uuid.Nil || s.accountMerchantID != p.MerchantID || s.accountPSPID != p.PSPID || s.accountSecret == "" || p.CustomerID == uuid.Nil || p.SessionID == uuid.Nil || !stripeEngineID(p.CustomerRef, "cus_") {
		return errors.New("Stripe setup account binding is incomplete")
	}
	return nil
}
func (s *StripeService) CreateEngineSetup(ctx context.Context, p StripeEngineSetupParams) (StripeEngineSetup, error) {
	if err := s.checkEngineSetup(p); err != nil {
		return StripeEngineSetup{}, err
	}
	scoped := *s
	scoped.Rails = railresolve.FixedSet{"stripe": {Rail: models.RailStripe, AccountID: s.accountID, Stripe: &config.StripeRailConfig{SecretKey: s.accountSecret}}}
	s = &scoped
	v := url.Values{"customer": {p.CustomerRef}, "usage": {"off_session"}, "payment_method_types[]": {"card"}}
	for k, value := range p.metadata() {
		v.Set("metadata["+k+"]", value)
	}
	body, err := s.stripePostForm(ctx, "/v1/setup_intents", v, "engine-setup:"+p.SessionID.String())
	if err != nil {
		return StripeEngineSetup{}, errors.New("Stripe setup outcome requires recovery")
	}
	return s.decodeEngineSetup(ctx, p, "", body)
}
func (s *StripeService) ReadEngineSetup(ctx context.Context, p StripeEngineSetupParams, reference string) (StripeEngineSetup, bool, error) {
	if err := s.checkEngineSetup(p); err != nil {
		return StripeEngineSetup{}, false, err
	}
	scoped := *s
	scoped.Rails = railresolve.FixedSet{"stripe": {Rail: models.RailStripe, AccountID: s.accountID, Stripe: &config.StripeRailConfig{SecretKey: s.accountSecret}}}
	s = &scoped
	if reference == "" {
		count := 0
		err := s.stripeListAll(ctx, "/v1/setup_intents", url.Values{"customer": {p.CustomerRef}}, func(raw json.RawMessage) error {
			var item struct {
				ID       string            `json:"id"`
				Metadata map[string]string `json:"metadata"`
			}
			if json.Unmarshal(raw, &item) != nil {
				return errors.New("Stripe setup list malformed")
			}
			if item.Metadata["openrails_setup_session"] == p.SessionID.String() {
				reference = item.ID
				count++
			}
			return nil
		})
		if err != nil {
			return StripeEngineSetup{}, false, err
		}
		if count > 1 {
			return StripeEngineSetup{}, false, errors.New("multiple Stripe setups claim one accepted session")
		}
		if count == 0 {
			return StripeEngineSetup{}, false, nil
		}
	}
	if !stripeEngineID(reference, "seti_") {
		return StripeEngineSetup{}, false, errors.New("invalid Stripe setup identity")
	}
	body, status, err := s.stripeGet(ctx, "/v1/setup_intents/"+url.PathEscape(reference), nil)
	if err != nil {
		return StripeEngineSetup{}, false, errors.New("Stripe setup read failed")
	}
	if status == http.StatusNotFound {
		return StripeEngineSetup{}, false, nil
	}
	if status >= 400 {
		return StripeEngineSetup{}, false, errors.New("Stripe setup read refused")
	}
	result, err := s.decodeEngineSetup(ctx, p, reference, body)
	return result, true, err
}
func (s *StripeService) decodeEngineSetup(ctx context.Context, p StripeEngineSetupParams, reference string, body []byte) (StripeEngineSetup, error) {
	var si struct {
		ID            string            `json:"id"`
		Status        string            `json:"status"`
		Customer      json.RawMessage   `json:"customer"`
		PaymentMethod json.RawMessage   `json:"payment_method"`
		Usage         string            `json:"usage"`
		LiveMode      *bool             `json:"livemode"`
		Metadata      map[string]string `json:"metadata"`
		ClientSecret  string            `json:"client_secret"`
		MethodTypes   []string          `json:"payment_method_types"`
	}
	if json.Unmarshal(body, &si) != nil || !stripeEngineID(si.ID, "seti_") || reference != "" && si.ID != reference || rawID(si.Customer) != p.CustomerRef || si.Usage != "off_session" || si.LiveMode == nil || *si.LiveMode == s.Config.IsTestMode() || len(si.MethodTypes) != 1 || si.MethodTypes[0] != "card" {
		return StripeEngineSetup{}, errors.New("Stripe setup differs from accepted customer and usage")
	}
	for k, v := range p.metadata() {
		if si.Metadata[k] != v {
			return StripeEngineSetup{}, errors.New("Stripe setup belongs to another operation")
		}
	}
	out := StripeEngineSetup{ID: si.ID, Status: si.Status, MethodRef: rawID(si.PaymentMethod)}
	if si.Status != "succeeded" {
		if si.Status == "requires_payment_method" || si.Status == "requires_confirmation" || si.Status == "requires_action" {
			if !strings.HasPrefix(si.ClientSecret, si.ID+"_secret_") {
				return out, errors.New("Stripe setup has no valid client authorization")
			}
			out.ClientSecret = si.ClientSecret
		}
		return out, nil
	}
	if !stripeEngineID(out.MethodRef, "pm_") {
		return out, errors.New("Stripe setup has no reusable payment method")
	}
	body, status, err := s.stripeGet(ctx, "/v1/payment_methods/"+url.PathEscape(out.MethodRef), nil)
	if err != nil || status >= 400 {
		return out, errors.New("Stripe setup card read failed")
	}
	var method struct {
		ID       string          `json:"id"`
		Type     string          `json:"type"`
		Customer json.RawMessage `json:"customer"`
		LiveMode *bool           `json:"livemode"`
		Card     *struct {
			LastFour string `json:"last4"`
			Brand    string `json:"brand"`
			ExpMonth int    `json:"exp_month"`
			ExpYear  int    `json:"exp_year"`
		} `json:"card"`
	}
	if json.Unmarshal(body, &method) != nil || method.ID != out.MethodRef || method.Type != "card" || rawID(method.Customer) != p.CustomerRef || method.LiveMode == nil || *method.LiveMode == s.Config.IsTestMode() || method.Card == nil || len(method.Card.LastFour) != 4 || method.Card.ExpMonth < 1 || method.Card.ExpMonth > 12 || method.Card.ExpYear < 2000 {
		return out, errors.New("Stripe setup card is not attached to accepted customer")
	}
	out.LastFour = method.Card.LastFour
	out.Brand = method.Card.Brand
	out.ExpMonth = method.Card.ExpMonth
	out.ExpYear = method.Card.ExpYear
	return out, nil
}
