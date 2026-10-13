package nmiproxy

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// VerifyCard stores a newly collected card's card-on-file reference without
// taking money. Selecting it for recurring billing requires separate consent.
func (c *Charger) VerifyCard(ctx context.Context, order string) (string, error) {
	if c == nil || c.BT == nil || c.Source.TokenIntentID == "" || c.Source.TokenID != "" {
		return "", errors.New("card setup requires a newly collected platform card")
	}
	req := charge.Request{Currency: "USD", OrderRef: order, Context: charge.Customer(charge.AgreementCardOnFile, nil)}
	form, err := paymentForm(req, c.Source, c.Gateway, nil, "validate")
	if err != nil {
		return "", err
	}
	if err := c.Gateway.Posture.RequireArmedFor(ctx, c.Gateway.directPostURL(), c.Gateway.SecurityKey); err != nil {
		return "", err
	}
	res, err := c.BT.ProxyForm(ctx, c.Gateway.directPostURL(), form)
	if err != nil {
		return "", err
	}
	verified, err := nmi.ParseSaleResponse(string(res.Body))
	if err != nil {
		return "", err
	}
	if verified.TransactionID == "" {
		return "", errors.New("card verification returned no transaction reference")
	}
	return verified.TransactionID, nil
}
