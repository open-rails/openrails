package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SEC-30: every card save, checkout and confirmation consults the durable
// failure ledger before the provider sees the card, and every refused card is
// counted, on whichever replica served it.

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
	wait, blocked, err := ledger.Blocked(r.Request.Context(), merchantID, abuse.CustomerSubject(customerID), abuse.AddressSubject(r.ClientIP()))
	if err != nil {
		log.WithError(err).WithField("request_id", r.RequestID()).Error("card attempt ledger unavailable")
		r.ErrorJSON(http.StatusServiceUnavailable, "card attempts are temporarily unavailable")
		return true
	}
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
// the durable ledger, then against this request's captcha subjects, with the
// ledger's attack verdict (#371). Best-effort: the response is unchanged when
// either write fails.
func recordCardFailure(r *httprequest.Request, subjects ...string) {
	if r == nil || r.State == nil {
		return
	}
	ctx := r.Request.Context()
	attack := false
	if ledger, merchantID, ok := cardAttemptLedger(r); ok {
		if err := ledger.Record(ctx, merchantID, subjects...); err != nil {
			log.WithError(err).WithField("request_id", r.RequestID()).Error("record card attempt failure")
		}
		if r.State.CardAbuseGuard != nil {
			var err error
			if attack, err = ledger.AttackMode(ctx, merchantID); err != nil {
				log.WithError(err).WithField("request_id", r.RequestID()).Error("card attack mode lookup")
			}
		}
	}
	id, _ := merchant.FromContext(ctx)
	r.State.CardAbuseGuard.RecordChargeFailure(ctx, id.UUID(), middleware.SubjectKeysFromContext(ctx), attack)
}
