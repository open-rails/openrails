package service

import (
	"net/http"

	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrBillingPolicyNotFound is a policy name the merchant never declared.
var ErrBillingPolicyNotFound = apperr.New(http.StatusNotFound, "billing_policy_not_found", "billing policy not found")
