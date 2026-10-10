package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/abuse"
)

// Every card save, checkout and confirmation consults the failure ledger
// before the provider sees the card, and every refused card is counted.

func cardAttemptLedger(r *httprequest.Request) (*abuse.FailureLedger, uuid.UUID, bool) {
	if r == nil || r.State == nil || r.State.CardFailureLedger == nil {
		return nil, uuid.Nil, false
	}
	id, ok := merchant.FromContext(r.Request.Context())
	if !ok || id.IsZero() {
		return nil, uuid.Nil, false
	}
	return r.State.CardFailureLedger, id.UUID(), true
}

// refuseBlockedCardAttempt answers 429 when the customer, the client address,
// or (in attack mode) any recently failing subject is blocked.
func refuseBlockedCardAttempt(r *httprequest.Request, customerID string) bool {
	ledger, merchantID, ok := cardAttemptLedger(r)
	if !ok {
		return false
	}
	wait, blocked := ledger.Blocked(r.Request.Context(), merchantID, abuse.CustomerSubject(customerID), abuse.AddressSubject(r.ClientIP()))
	if !blocked {
		return false
	}
	writeCardAttemptsBlocked(r, wait)
	return true
}

func writeCardAttemptsBlocked(r *httprequest.Request, wait time.Duration) {
	r.SetHeader("Retry-After", strconv.FormatInt(int64((wait+time.Second-1)/time.Second), 10))
	r.APIError(api.NewAPIError(http.StatusTooManyRequests, api.ErrorTypeRateLimit, "card_attempts_blocked",
		"Too many declined card attempts. Try again later."))
}

// recordCardFailure counts one card the provider refused: against subjects in
// the ledger, then against this request's captcha subjects, with the ledger's
// attack verdict.
func recordCardFailure(r *httprequest.Request, subjects ...string) {
	if r == nil || r.State == nil {
		return
	}
	ctx := r.Request.Context()
	attack := false
	if ledger, merchantID, ok := cardAttemptLedger(r); ok {
		ledger.Record(ctx, merchantID, subjects...)
		if r.State.CardAbuseGuard != nil {
			attack = ledger.AttackMode(ctx, merchantID)
		}
	}
	id, _ := merchant.FromContext(ctx)
	r.State.CardAbuseGuard.RecordChargeFailure(ctx, id.UUID(), middleware.SubjectKeysFromContext(ctx), attack)
}
