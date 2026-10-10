// Package idguard refuses blank and zero identifiers at engine entry points,
// before any lookup. A zero UUID is a supplied value, not "unset": treated as
// absent it silently selects a default, and sent to the database it becomes a
// "not found" probe; either way a malformed request escapes "invalid".
package idguard

import (
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Invalid is the coded invalid-parameter refusal: HTTP 400, code
// invalid_param, naming the offending field. errors.Is(err, billing.ErrInvalid)
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
func RequireMerchant(field string, id billing.MerchantID) error {
	if id.IsZero() {
		return Invalid(field, field+" is required")
	}
	return nil
}
