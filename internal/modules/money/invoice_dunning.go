package money

import (
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/payments"
)

const invoiceCollectionMaxFailures int32 = 5

var invoiceCollectionRetryOffsets = [...]time.Duration{
	2 * 24 * time.Hour,
	5 * 24 * time.Hour,
	9 * 24 * time.Hour,
	13 * 24 * time.Hour,
}

type invoiceCollectionFailureAction struct {
	nextAttemptAt *time.Time
	terminal      bool
}

func invoiceCollectionAction(rail string, failureCode *string, currentFailures int32, firstFailureAt *time.Time, now time.Time) invoiceCollectionFailureAction {
	if invoiceCollectionNeedsPaymentMethod(rail, failureCode) {
		return invoiceCollectionFailureAction{}
	}
	nextFailureCount := currentFailures + 1
	if nextFailureCount >= invoiceCollectionMaxFailures {
		return invoiceCollectionFailureAction{terminal: true}
	}
	first := now
	if firstFailureAt != nil {
		first = *firstFailureAt
	}
	next := first.Add(invoiceCollectionRetryOffsets[nextFailureCount-1])
	return invoiceCollectionFailureAction{nextAttemptAt: &next}
}

func invoiceCollectionNeedsPaymentMethod(rail string, failureCode *string) bool {
	if failureCode == nil {
		return true
	}
	raw := strings.ToLower(strings.TrimSpace(*failureCode))
	switch payments.NormalizeFailureReason(strings.ToLower(strings.TrimSpace(rail)), raw) {
	case payments.FailureInsufficientFunds,
		payments.FailureCardDeclined,
		payments.FailureProcessorError,
		payments.FailureGenericDecline:
		return false
	default:
		return true
	}
}
