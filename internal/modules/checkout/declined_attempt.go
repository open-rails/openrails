package checkout

import (
	"context"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// DeclinedAttempt is a parsed hard decline on a checkout charge path (#796):
// every charge attempt must land in openrails.payments — a decline that only
// errors the browser silently inflates approval_rate.
type DeclinedAttempt struct {
	UserID  string
	PriceID uuid.UUID
	Rail    string
	// SyntheticTransactionID is the idempotent per-attempt row key (declines
	// have no settled gateway charge to key on; e.g. "nmi_sale_declined:<intent>").
	SyntheticTransactionID string
	// AmountMicros/Currency of the attempted charge.
	AmountMicros int64
	Currency     string
	// FailureCode is the rail's decline code VERBATIM (#651 no fabrication).
	FailureCode string
	AttemptKind string // payments.AttemptInitial | payments.AttemptRenewal
	TokenType   string // charge.TokenType* (#796); "" = unknown
}

// recordDeclinedAttempt writes the failed payments row. Best-effort by
// design: the decline outcome must reach the user even if metrics recording
// fails — errors are logged, never returned. Idempotent on the synthetic
// transaction id.
func recordDeclinedAttempt(ctx context.Context, paymentService *payments.PaymentService, a DeclinedAttempt) {
	if paymentService == nil {
		log.WithContext(ctx).Warn("declined attempt not recorded: payment service unavailable")
		return
	}
	customerID, err := customerIDFromUser(a.UserID)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("declined attempt not recorded: bad user id")
		return
	}
	kind := strings.TrimSpace(a.AttemptKind)
	if kind == "" {
		kind = payments.AttemptInitial
	}
	failed := &models.Payment{
		ID:            uuidutil.NewV7(),
		CustomerID:    customerID,
		PriceID:       a.PriceID,
		Rail:          models.Rail(strings.ToLower(strings.TrimSpace(a.Rail))),
		TransactionID: a.SyntheticTransactionID,
		Amount:        a.AmountMicros,
		ListAmount:    a.AmountMicros,
		Currency:      a.Currency,
		Status:        payments.PaymentStatusFailedValue,
		AttemptKind:   &kind,
	}
	if code := strings.TrimSpace(a.FailureCode); code != "" {
		reason := payments.NormalizeFailureReason(string(failed.Rail), code)
		failed.FailureCode = &code
		failed.FailureReason = &reason
	}
	if tt := strings.TrimSpace(a.TokenType); tt != "" {
		failed.TokenType = &tt
	}
	if _, err := paymentService.CreateIfNotExists(ctx, failed); err != nil {
		log.WithContext(ctx).WithError(err).WithField("transaction_id", a.SyntheticTransactionID).
			Error("failed to record declined checkout attempt (#796)")
	}
}
