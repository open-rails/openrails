package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// payoutID is a Stripe payout id as stripe_payout_reads holds it.
var payoutID = regexp.MustCompile(`^po_[A-Za-z0-9_]{1,250}$`)

// queuePayoutRead queues the books' read of the payout a notice names, at
// once and its failures forgotten (payouts.StripeReader).
func (s *StripeWebhookService) queuePayoutRead(ctx context.Context, eventType string, obj json.RawMessage) error {
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(obj, &data); err != nil {
		return fmt.Errorf("parse %s: %w", eventType, err)
	}
	id := strings.TrimSpace(data.ID)
	if !payoutID.MatchString(id) {
		s.Log.InfoContext(ctx, "stripe payout event without a payout id; nothing to read", "event_type", eventType)
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	return s.DB.Gen(ctx).QueueStripePayoutRead(ctx, gen.QueueStripePayoutReadParams{MerchantID: mid.UUID(), PspID: psp, PayoutID: id})
}
