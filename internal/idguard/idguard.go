// Package idguard refuses blank and zero identifiers at engine entry points,
// before any lookup or provider consideration.
//
// A zero UUID is not "unset": it is a value the caller supplied. Treating it as
// absent makes an explicit zero mean "use the default" (a planner silently
// re-targeting the subscription's own account), and letting it reach the
// database turns it into a "not found" probe — both answer a malformed request
// with something other than "that parameter is invalid". #479 already refuses
// uuid.Nil when parsing a prefixed wire id; these guards give plain-UUID and
// durable-payload inputs the same treatment.
package idguard

import (
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Invalid is the coded invalid-parameter refusal: HTTP 400, code
// invalid_param, naming the offending field. errors.Is(err, openrails.ErrInvalid)
// classifies it identically in embedded and remote callers.
func Invalid(field, message string) error {
	return apperr.Invalidf("%s", message).WithParam(field)
}

// Require refuses a zero UUID.
func Require(field string, id uuid.UUID) error {
	if id == uuid.Nil {
		return Invalid(field, field+" is required")
	}
	return nil
}

// RequireOptional refuses a supplied-but-zero UUID. A nil pointer is genuinely
// "not supplied" and passes.
func RequireOptional(field string, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	return Require(field, *id)
}

// RequireMerchant refuses a missing or zero merchant scope.
func RequireMerchant(field string, id merchant.ID) error {
	if id.IsZero() {
		return Invalid(field, field+" is required")
	}
	return nil
}
