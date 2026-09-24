package handlers

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/pkg/api"
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
	r.SetHeader("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	r.APIError(api.NewAPIError(http.StatusTooManyRequests, api.ErrorTypeRateLimit, "card_attempts_blocked",
		"Too many declined card attempts. Try again later."))
}

// recordCardFailure counts one refused card attempt against subjects.
// Best-effort: the response is unchanged when the ledger write fails.
func recordCardFailure(r *httprequest.Request, subjects ...string) {
	ledger, merchantID, ok := cardAttemptLedger(r)
	if !ok {
		return
	}
	if err := ledger.Record(r.Request.Context(), merchantID, subjects...); err != nil {
		log.WithError(err).WithField("request_id", r.RequestID()).Error("record card attempt failure")
	}
}
