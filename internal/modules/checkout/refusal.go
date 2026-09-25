package checkout

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// ErrPaymentMethodStale is a checkout whose saved payment method can no longer
// be charged for this customer and processor: it is gone, not theirs, vaulted
// on another provider account, or its collection token expired. Nothing was
// charged; the card must be collected again.
var ErrPaymentMethodStale = errors.New("saved payment method can no longer be charged")

// ErrPaymentMethodRequired is a card checkout that names neither a saved
// method nor a new card token. The customer's default card is never implied.
var ErrPaymentMethodRequired = fmt.Errorf("%w: payment_method_id or payment_token is required", ErrCheckoutSessionValidation)

// terminalCheckoutError turns a failed_terminal checkout intent into the typed
// refusal its evidence proves. A provider decline becomes a
// paymentmethods.PaymentMethodError carrying the verbatim failure code; a
// stale instrument becomes ErrPaymentMethodStale; any other terminal outcome
// (request rejected before the provider answered, verified non-execution)
// stays an opaque failure.
func terminalCheckoutError(intent gen.OpenrailsRailIntent, prefix string) error {
	reason := prefix
	if intent.LastFailureReason != nil && strings.TrimSpace(*intent.LastFailureReason) != "" {
		reason = prefix + ": " + *intent.LastFailureReason
	}
	var evidence struct {
		Declined       bool   `json:"declined"`
		LocalizationID string `json:"localization_id"`
		ResponseCode   int    `json:"response_code"`
		FailureCode    string `json:"failure_code"`
		DeclineCode    string `json:"decline_code"`
		Stale          bool   `json:"payment_method_stale"`
	}
	if len(intent.ResultEvidence) > 0 {
		_ = json.Unmarshal(intent.ResultEvidence, &evidence)
	}
	switch {
	case evidence.Stale:
		return fmt.Errorf("%w: %s", ErrPaymentMethodStale, reason)
	case evidence.Declined:
		code := strings.TrimSpace(evidence.LocalizationID)
		if code == "" {
			code = strings.TrimSpace(evidence.FailureCode)
		}
		if code == "" && evidence.ResponseCode != 0 {
			code = strconv.Itoa(evidence.ResponseCode)
		}
		if code == "" {
			code = strings.TrimSpace(evidence.DeclineCode)
		}
		return &paymentmethods.PaymentMethodError{Err: errors.New(reason), LocalizationID: code, Message: reason, Rail: intent.Rail, Failure: operationFailure(intent)}
	default:
		return errors.New(reason)
	}
}
