package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
)

// PaymentToAPI is a payment on the wire; refunds, when not nil, are the
// reversals of a charge and set its refunded amount.
func PaymentToAPI(p *models.Payment, refunds []*models.Payment) billing.Payment {
	var amountRefunded int64
	out := make([]billing.Payment, 0, len(refunds))
	for _, r := range refunds {
		if r.Status == "completed" {
			amountRefunded += abs(r.Amount)
		}
		out = append(out, paymentView(r, 0))
	}
	payment := paymentView(p, amountRefunded)
	if refunds != nil {
		payment.Refunds = out
	}
	return payment
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// paymentView is a payment on the wire, given the completed refunds against
// it. A charge refunded in full reads refunded, in part partially_refunded.
func paymentView(p *models.Payment, amountRefunded int64) billing.Payment {
	out := billing.Payment{
		ID:             billing.PaymentID(p.ID),
		Kind:           paymentKind(p),
		Status:         paymentStatus(p.Status),
		Amount:         p.Amount,
		AmountRefunded: amountRefunded,
		Currency:       p.Currency,
		CustomerID:     billing.CustomerID(p.CustomerID),
		PriceID:        billing.PriceID(p.PriceID),
		Channel:        billing.ChannelRail,
		TransactionID:  p.TransactionID,
		CreatedAt:      p.CreatedAt,
	}
	if p.SubscriptionID != nil {
		id := billing.SubscriptionID(*p.SubscriptionID)
		out.SubscriptionID = &id
	}
	if p.Price != nil {
		out.Price = p.Price.PublicView()
		if p.Price.Product != nil {
			out.Product = p.Price.Product.Summary()
		}
	}
	switch channel := models.Channel(p.Rail); channel {
	case models.ChannelManual, models.ChannelAdmin:
		out.Channel = billing.PaymentChannel(channel)
	default:
		rail := string(p.Rail)
		out.Rail = &rail
	}
	if p.PspID != nil {
		psp := billing.PSPID(*p.PspID)
		out.PSPID = &psp
	}
	card := models.Card{}
	if p.CardBrand != nil {
		card.Brand = *p.CardBrand
	}
	if p.CardLast4 != nil {
		card.Last4 = *p.CardLast4
	}
	out.Card = card.Details()
	if p.RefundedPaymentID != nil {
		original := billing.PaymentID(*p.RefundedPaymentID)
		out.RefundedPaymentID = &original
		if reason := adminRefundMetadataString(p.Metadata, "admin_refund_reason"); reason != "" {
			out.Reason = &reason
		}
	}
	if out.Kind == billing.PaymentCharge && out.Status == billing.PaymentSucceeded && amountRefunded > 0 {
		out.Status = billing.PaymentPartiallyRefunded
		if amountRefunded >= p.Amount {
			out.Status = billing.PaymentRefunded
		}
	}
	if out.Status == billing.PaymentFailed {
		code := ""
		if p.FailureCode != nil {
			code = *p.FailureCode
		}
		failure := decline.Classify(string(p.Rail), code).Reason.Failure()
		out.Failure = &failure
	}
	return out
}

func paymentKind(p *models.Payment) billing.PaymentKind {
	switch {
	case p.ReversalKind != nil && *p.ReversalKind != "":
		return billing.PaymentKind(*p.ReversalKind)
	case p.RefundedPaymentID != nil || p.Amount < 0:
		return billing.PaymentRefund
	}
	return billing.PaymentCharge
}

func paymentStatus(status string) billing.PaymentStatus {
	switch status {
	case "completed":
		return billing.PaymentSucceeded
	case "refunded":
		return billing.PaymentRefunded
	case "failed":
		return billing.PaymentFailed
	}
	return billing.PaymentPending
}
