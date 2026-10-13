package intents

import (
	"context"
	"encoding/json"

	"github.com/open-rails/openrails/internal/db/gen"
	log "github.com/sirupsen/logrus"
)

type orderCaptchaSubjectsKey struct{}

// WithOrderCaptchaSubjects carries the HTTP limiter's subjects into an order
// payment's admission. Workers use the stored subjects, never their own address.
func WithOrderCaptchaSubjects(ctx context.Context, subjects []string) context.Context {
	return context.WithValue(ctx, orderCaptchaSubjectsKey{}, subjects)
}

func OrderCaptchaSubjects(ctx context.Context) []string {
	subjects, _ := ctx.Value(orderCaptchaSubjectsKey{}).([]string)
	return subjects
}

func (r *Runner) recordOrderCardDecline(ctx context.Context, in gen.BillingProviderIntent) {
	if r.CardAbuseGuard == nil || in.Origin != string(OriginUser) {
		return
	}
	switch in.IntentType {
	case "nmi_sale", "subscription_collection", TypeOrderCollection:
	default:
		return
	}
	var payload struct {
		OrderCaptchaSubjects []string `json:"order_captcha_subjects"`
	}
	if err := json.Unmarshal(in.Payload, &payload); err != nil {
		log.WithContext(ctx).WithError(err).WithField("intent_id", in.ID).Error("cannot read order captcha subjects")
		return
	}
	r.CardAbuseGuard.RecordOrderDecline(ctx, in.MerchantID, in.ID, payload.OrderCaptchaSubjects, r.CardFailureLedger.AttackMode(ctx, in.MerchantID))
}
