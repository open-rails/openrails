// Package nmiproxy implements the charge seam for NMI charges whose card a
// third-party custodian holds: Basis Theory's detokenizing proxy presents the
// stored card to NMI's Direct Post gateway. Same rail and decline taxonomy as
// nmidirect; only the transport differs. The PAN never touches OpenRails (SAQ
// A): the engine holds only the custodian token reference and the NMI
// security key. As in nmidirect, NMI's transactionid is the stored-credential
// reference.
//
// The BT proxy has no idempotency support: retry safety rests on the durable
// intents log, NMI duplicate detection (430 = transient) and the orderid
// verify leg. Ambiguous outcomes (basistheory.IsTransportAmbiguous,
// nmi.IsTransportAmbiguous) are verified, never blind-retried or declined.
package nmiproxy

import (
	"context"
	"errors"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Rail is NMI: the proxy changes how the card reaches the gateway, never which
// gateway charges it.
const Rail = string(models.RailNMI)

// Custodian is the payment_methods.custodian of these rows: the PAN is held
// at Basis Theory while the processor stays NMI.
const Custodian = models.CustodianBasisTheory

// Charger charges custodian-held instruments through the seam. Source selects the
// credential per charge (callers construct one Charger per charge site from
// the instrument row); Instrument.MethodRef is the fallback token id when
// Source is zero.
type Charger struct {
	BT      *basistheory.Client
	Gateway GatewayConfig
	Source  Source
}

func New(bt *basistheory.Client, gw GatewayConfig) *Charger {
	return &Charger{BT: bt, Gateway: gw}
}

// WithSource returns a copy routed at one credential source.
func (c *Charger) WithSource(src Source) *Charger {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Source = src
	return &cp
}

var _ charge.Charger = (*Charger)(nil)

func (c *Charger) Charge(ctx context.Context, req charge.Request) (charge.Result, error) {
	if err := moneyutil.RequireFiatCurrency(req.Currency); err != nil {
		return charge.Result{}, errors.Join(charge.ErrNotDispatched, err)
	}
	if c == nil || c.BT == nil {
		return charge.Result{}, errors.New("nmiproxy charger not initialized")
	}
	src := c.Source
	if strings.TrimSpace(src.TokenID) == "" && strings.TrimSpace(src.TokenIntentID) == "" {
		src.TokenID = strings.TrimSpace(req.Instrument.MethodRef)
	}

	if src.via() == ViaNetworkToken {
		res, err := c.chargeNetworkToken(ctx, req, src)
		if err == nil {
			return res, nil
		}
		// NT-side failure BEFORE the destination answered (cryptogram 4xx, BT
		// pre-forward error): fall back to pan_proxy in the SAME attempt, same
		// orderid. Ambiguous outcomes never fall back — the charge may exist.
		if canFallBackToPAN(err) && strings.TrimSpace(src.TokenID) != "" {
			log.WithContext(ctx).WithError(err).WithField("network_token_id", src.NetworkTokenID).
				Warn("nmiproxy: network-token charge failed pre-forward; falling back to pan_proxy in the same attempt")
			fallback := src
			fallback.Via = ViaPANProxy
			return c.chargeThroughProxy(ctx, req, fallback, nil)
		}
		return res, err
	}
	return c.chargeThroughProxy(ctx, req, src, nil)
}

func (c *Charger) chargeNetworkToken(ctx context.Context, req charge.Request, src Source) (charge.Result, error) {
	var cryptogram *basistheory.Cryptogram
	if req.Context.Initiator == charge.InitiatorCustomer {
		// Cryptograms are single-use and short-lived: minted per attempt.
		cg, err := c.BT.CreateCryptogram(ctx, src.NetworkTokenID)
		if err != nil {
			return charge.Result{}, err
		}
		cryptogram = cg
	}
	return c.chargeThroughProxy(ctx, req, src, cryptogram)
}

// canFallBackToPAN: only clean BT-side failures where the destination was
// provably never reached may retry as pan_proxy within the attempt.
func canFallBackToPAN(err error) bool {
	if err == nil {
		return false
	}
	if basistheory.IsTransportAmbiguous(err) || nmi.IsTransportAmbiguous(err) {
		return false
	}
	if errors.Is(err, basistheory.ErrProviderReadOnly) {
		return false
	}
	if _, ok := basistheory.IsBTProxyError(err); ok {
		return true
	}
	var apiErr *basistheory.APIError
	return errors.As(err, &apiErr) // cryptogram/NT API rejections
}

func (c *Charger) chargeThroughProxy(ctx context.Context, req charge.Request, src Source, cryptogram *basistheory.Cryptogram) (charge.Result, error) {
	form, err := SaleForm(req, src, c.Gateway, cryptogram)
	if err != nil {
		return charge.Result{}, err
	}
	if err := c.Gateway.Posture.RequireArmedFor(ctx, c.Gateway.directPostURL(), c.Gateway.SecurityKey); err != nil {
		return charge.Result{}, errors.Join(charge.ErrNotDispatched, err)
	}
	proxyRes, err := c.BT.ProxyForm(ctx, c.Gateway.directPostURL(), form)
	if err != nil {
		// Ambiguous (may have forwarded), BT pre-forward failure (clean,
		// NOT a decline), or readonly — all the caller's to classify.
		return charge.Result{}, err
	}

	tokenType := charge.TokenTypePANViaProxy
	if src.via() == ViaNetworkToken {
		tokenType = charge.TokenTypeNetworkToken
	}

	// Destination answered: same parser + decline taxonomy as the direct rail.
	sale, err := nmi.ParseSaleResponse(string(proxyRes.Body))
	if err != nil {
		var vaultErr *nmi.CustomerVaultError
		if errors.As(err, &vaultErr) && nmidirect.IsHardDecline(vaultErr.ResponseCode) {
			code := nmidirect.FailureCode(vaultErr)
			message := vaultErr.Error()
			return charge.Result{
				TokenType:      tokenType,
				Declined:       true,
				FailureCode:    &code,
				FailureMessage: &message,
			}, nil
		}
		// Transient gateway condition (420/421/430) or unreadable body
		// (transport-ambiguous): the caller's retry/verify machinery owns it.
		return charge.Result{}, err
	}

	res := charge.Result{
		TransactionID: strings.TrimSpace(sale.TransactionID),
		TokenType:     tokenType,
	}
	// An approved storing transaction is its agreement's lineage (as in
	// nmidirect: NMI's transactionid).
	if req.Context.Storing() {
		res.CapturedRef = res.TransactionID
	}
	return res, nil
}
