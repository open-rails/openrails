package nmiproxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
)

// Charge routing values (payment_methods.charge_via).
const (
	ViaPANProxy     = "pan_proxy"
	ViaNetworkToken = "network_token"
)

// GatewayConfig is the destination NMI account: OpenRails holds the
// security_key (it transits the BT proxy verbatim); the PAN never does.
type GatewayConfig struct {
	SecurityKey   string
	DirectPostURL string // "" = nmi.DefaultDirectPostURL
}

func (g GatewayConfig) directPostURL() string {
	if u := strings.TrimSpace(g.DirectPostURL); u != "" {
		return u
	}
	return nmi.DefaultDirectPostURL
}

// Source selects the BT credential one charge detokenizes. Exactly one of
// TokenID / TokenIntentID is the PAN source; NetworkTokenID rides Via.
type Source struct {
	// TokenID: durable stored card token (MITs, CVC-less CIT reuse).
	TokenID string
	// TokenIntentID: checkout CIT charging the 24h intent (CVC present —
	// the CVC lives on the INTENT and never moves to the token).
	TokenIntentID string
	// Via routes pan_proxy | network_token ("" = pan_proxy).
	Via string
	// NetworkTokenID is required when Via = network_token.
	NetworkTokenID string
}

func (s Source) via() string {
	if strings.TrimSpace(s.Via) == "" {
		return ViaPANProxy
	}
	return strings.TrimSpace(s.Via)
}

// expression composition — pure, wire-pinned by unit test. Roots and filters
// are BT's documented detokenization grammar verbatim.

func tokenNumberExpr(tokenID string) string {
	return fmt.Sprintf(`{{ token: %s | json: "$.data.number" }}`, tokenID)
}
func tokenExpExpr(tokenID string) string {
	return fmt.Sprintf(`{{ token: %s | json: "$.data" | card_exp: "MMYY" }}`, tokenID)
}
func intentNumberExpr(intentID string) string {
	return fmt.Sprintf(`{{ token_intent: %s | json: "$.data.number" }}`, intentID)
}
func intentExpExpr(intentID string) string {
	return fmt.Sprintf(`{{ token_intent: %s | json: "$.data" | card_exp: "MMYY" }}`, intentID)
}
func intentCVCExpr(intentID string) string {
	return fmt.Sprintf(`{{ token_intent: %s | json: "$.data.cvc" }}`, intentID)
}
func networkTokenNumberExpr(ntID string) string {
	return fmt.Sprintf(`{{ network_token: %s | json: "$.data.number" }}`, ntID)
}
func networkTokenExpExpr(ntID string) string {
	return fmt.Sprintf(`{{ network_token: %s | json: "$.data" | card_exp: "MMYY" }}`, ntID)
}

// SaleForm composes the classic NMI DirectPost sale form with BT
// detokenization expressions. Pure: no I/O, no defaults fabricated —
// missing amount/currency/source is a loud error (#651). cryptogram is
// required for network-token CITs (nil otherwise).
func SaleForm(req charge.Request, src Source, gw GatewayConfig, cryptogram *basistheory.Cryptogram) (url.Values, error) {
	if strings.TrimSpace(gw.SecurityKey) == "" {
		return nil, errors.New("nmiproxy: gateway security_key is required")
	}
	if req.AmountMinor <= 0 {
		return nil, errors.New("nmiproxy: charge amount must be positive")
	}
	currency := strings.TrimSpace(req.Currency)
	if currency == "" {
		return nil, errors.New("nmiproxy: currency is required")
	}
	if len(req.OrderRef) > 50 {
		return nil, fmt.Errorf("nmiproxy: order id %q exceeds NMI's 50-character limit", req.OrderRef)
	}
	sc := nmidirect.StoredCredentialFor(req.Context)
	if err := sc.Validate(); err != nil {
		return nil, err
	}

	values := url.Values{
		"type":         {"sale"},
		"security_key": {gw.SecurityKey},
		"amount":       {nmi.WireAmount(req.AmountMinor)},
		"currency":     {currency},
	}
	if desc := strings.TrimSpace(req.Description); desc != "" {
		values.Set("order_description", desc)
	}
	if req.OrderRef != "" {
		values.Set("orderid", req.OrderRef)
	}

	tokenID := strings.TrimSpace(src.TokenID)
	intentID := strings.TrimSpace(src.TokenIntentID)
	switch src.via() {
	case ViaNetworkToken:
		ntID := strings.TrimSpace(src.NetworkTokenID)
		if ntID == "" {
			return nil, errors.New("nmiproxy: charge_via=network_token requires a network token id")
		}
		values.Set("ccnumber", networkTokenNumberExpr(ntID))
		values.Set("ccexp", networkTokenExpExpr(ntID))
		// CIT: cryptogram + ECI via the 3DS passthrough fields; MIT rides the
		// stored-credential initial_transaction_id with no cryptogram.
		if req.Context.Initiator == charge.InitiatorCustomer {
			if cryptogram == nil || strings.TrimSpace(cryptogram.Cryptogram) == "" {
				return nil, errors.New("nmiproxy: network-token CIT requires a cryptogram")
			}
			values.Set("cavv", cryptogram.Cryptogram)
			if eci := strings.TrimSpace(cryptogram.ECI); eci != "" {
				values.Set("eci", eci)
			}
		}
	case ViaPANProxy:
		switch {
		case intentID != "":
			if req.Context.Initiator != charge.InitiatorCustomer {
				return nil, errors.New("nmiproxy: token intents are checkout CIT sources; MITs charge the stored token")
			}
			values.Set("ccnumber", intentNumberExpr(intentID))
			values.Set("ccexp", intentExpExpr(intentID))
			values.Set("cvv", intentCVCExpr(intentID))
		case tokenID != "":
			values.Set("ccnumber", tokenNumberExpr(tokenID))
			values.Set("ccexp", tokenExpExpr(tokenID))
			// No cvv: card-token CVC auto-deletes (~1h retention); MITs and
			// token reuses are CVC-less by design.
		default:
			return nil, errors.New("nmiproxy: charge source requires a token or token intent id")
		}
	default:
		return nil, fmt.Errorf("nmiproxy: unknown charge_via %q", src.Via)
	}

	sc.ApplyToForm(values)
	return values, nil
}
