package intents

import (
	"context"
	"encoding/json"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
)

type invoiceCaptchaSubjectsKey struct{}

// WithInvoiceCaptchaSubjects carries the HTTP limiter's subjects into an invoice
// payment's admission. Workers use the stored subjects, never their own address.
func WithInvoiceCaptchaSubjects(ctx context.Context, subjects []string) context.Context {
	return context.WithValue(ctx, invoiceCaptchaSubjectsKey{}, subjects)
}

func InvoiceCaptchaSubjects(ctx context.Context) []string {
	subjects, _ := ctx.Value(invoiceCaptchaSubjectsKey{}).([]string)
	return subjects
}

func (r *Runner) recordInvoiceCardDecline(ctx context.Context, in gen.BillingProviderIntent) {
	if r.CardAbuseGuard == nil || in.Origin != string(OriginUser) {
		return
	}
	switch in.IntentType {
	case "nmi_sale", "subscription_collection", "manual_rebill", TypeInvoiceCollection:
	default:
		return
	}
	var payload struct {
		InvoiceCaptchaSubjects []string `json:"order_captcha_subjects"`
	}
	if err := json.Unmarshal(in.Payload, &payload); err != nil {
		r.Log.ErrorContext(ctx, "cannot read invoice captcha subjects", logx.Err(err), "intent_id", in.ID)
		return
	}
	r.CardAbuseGuard.RecordInvoiceDecline(ctx, in.MerchantID, in.ID, payload.InvoiceCaptchaSubjects, r.CardFailureLedger.AttackMode(ctx, in.MerchantID))
}
