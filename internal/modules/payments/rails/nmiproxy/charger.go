// Package nmiproxy implements the rail-agnostic charge seam (#297 Phase B /
// #795) for NMI charges whose card is held by a third-party CUSTODIAN: Basis
// Theory's ephemeral detokenizing proxy presents the stored card to the NMI
// DirectPost gateway. Same rail as nmidirect, same decline taxonomy — only the
// transport differs (or#879). The PAN never touches OpenRails (SAQ A); the
// engine holds only the custodian token reference
// (payment_methods.rail_method_ref) and the NMI security key.
//
// Anchor semantics are IDENTICAL to nmidirect: NMI's gateway transactionid is
// the stored-credential replay reference — NMI-scoped, so instruments survive
// a future vault swap per-agreement-sequence.
//
// Retry safety: the BT proxy has NO idempotency support. Safety rests on the
// durable intents log (#674), NMI duplicate detection (430 = transient), and
// the orderid verify leg. Ambiguous outcomes (basistheory.IsTransportAmbiguous
// or nmi.IsTransportAmbiguous) must be VERIFIED, never blind-retried and never
// treated as declines.
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
)

// Rail is the rail vocabulary value (payment_methods.rail / payments.rail).
// It is NMI: the proxy changes how the card reaches the gateway, never which
// gateway charges it (or#879).
const Rail = string(models.RailNMI)

// Custodian is the payment_methods.custodian value for these rows (or#880):
// the PAN is held at Basis Theory while the processor stays NMI.
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
	// Reference-less charge anchors the agreement sequence (identical to
	// nmidirect: the replay ref IS NMI's transactionid).
	if strings.TrimSpace(req.Context.PriorRef) == "" {
		res.CapturedRef = res.TransactionID
	}
	return res, nil
}
