package billing

import (
	"net/http"
	"sort"
)

// Error envelope types: the category in ErrorDetails.Type.
const (
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeAuthentication = "authentication_error"
	ErrorTypeAuthorization  = "authorization_error"
	ErrorTypeCard           = "card_error"
	ErrorTypeRateLimit      = "rate_limit_error"
	ErrorTypeAPI            = "api_error"
)

// ErrorCode is one registered wire error code: the ErrorDetails.Code value,
// the HTTP status and envelope type that always accompany it, and what it
// means. Codes are stable; messages are not.
type ErrorCode struct {
	Code    string
	Status  int
	Type    string
	Meaning string
}

// Generic codes: what a refusal answers when nothing more specific applies.
const (
	CodeInvalidParam           = "invalid_param"
	CodeAuthenticationRequired = "authentication_required"
	CodeResourceAccessDenied   = "resource_access_denied"
	CodeResourceNotFound       = "resource_not_found"
	CodeResourceConflict       = "resource_conflict"
	CodePaymentFailed          = "payment_failed"
	CodeRateLimitExceeded      = "rate_limit_exceeded"
	CodeInternalError          = "internal_error"
	CodeServiceUnavailable     = "service_unavailable"
	CodeIdempotencyKeyReused   = "idempotency_key_reused"
	CodeInsufficientCredits    = "insufficient_credits"
	CodeInsufficientFunds      = "insufficient_funds"
)

// Request-shape codes: the request never reached its operation.
const (
	// CodeUnknownField: the JSON body names a field the route does not
	// accept; param is the field.
	CodeUnknownField = "unknown_field"
	// CodeInvalidQuery: a query parameter is malformed or out of range;
	// param is the parameter.
	CodeInvalidQuery = "invalid_query"
	// CodeInvalidCursor: the list cursor is not one this list issued.
	CodeInvalidCursor                = "invalid_cursor"
	CodeUnsupportedMediaType         = "unsupported_media_type"
	CodeRouteNotFound                = "route_not_found"
	CodeMethodNotAllowed             = "method_not_allowed"
	CodeMerchantSelectorInvalid      = "merchant_selector_invalid"
	CodeMerchantNotFound             = "merchant_not_found"
	CodeMerchantBindingMismatch      = "merchant_binding_mismatch"
	CodeMerchantDirectoryUnavailable = "merchant_directory_unavailable"
)

// Authentication and authorization codes: why a credential was refused.
const (
	CodeCredentialExpired                     = "credential_expired"
	CodeCredentialRevoked                     = "credential_revoked"
	CodeCredentialIdentityMismatch            = "credential_identity_mismatch"
	CodeSenderProofRequired                   = "sender_proof_required"
	CodeServiceCredentialInvalid              = "service_credential_invalid"
	CodeServiceCredentialMerchantUnresolved   = "service_credential_merchant_unresolved"
	CodeServiceCredentialResourceScopeDenied  = "service_credential_resource_scope_denied"
	CodeServiceCredentialCustomerScopeDenied  = "service_credential_customer_scope_denied"
	CodeDelegatedTokenInvalid                 = "delegated_token_invalid"
	CodeDelegatedTokenExpired                 = "delegated_token_expired"
	CodeDelegatedTokenRevoked                 = "delegated_token_revoked"
	CodeDelegatedPrincipalInvalid             = "delegated_principal_invalid"
	CodeDelegatedMerchantUnresolved           = "delegated_merchant_unresolved"
	CodeDelegatedVerificationUnavailable      = "delegated_verification_unavailable"
	CodeHostPrincipalInvalid                  = "host_principal_invalid"
	CodePermissionRequired                    = "permission_required"
	CodeMerchantUnresolved                    = "merchant_unresolved"
	CodeHostMerchantMismatch                  = "host_merchant_mismatch"
	CodeMerchantContextMismatch               = "merchant_context_mismatch"
	CodeInvokerScopedPrincipal                = "invoker_scoped_principal"
	CodeCatalogOwnerRequired                  = "catalog_owner_required"
	CodeStepUpRequired                        = "step_up_required"
	CodeStepUpUnavailable                     = "step_up_unavailable"
	CodeAuthenticationUnavailable             = "authentication_unavailable"
	CodeAuthorizationUnavailable              = "authorization_unavailable"
	CodeMerchantCreationPaymentMethodRequired = "merchant_creation_payment_method_required"
)

const (
	invalid = ErrorTypeInvalidRequest
	authn   = ErrorTypeAuthentication
	authz   = ErrorTypeAuthorization
	card    = ErrorTypeCard
	limit   = ErrorTypeRateLimit
	fault   = ErrorTypeAPI
)

// errorCodes is the registry: every code the HTTP API answers. A code has one
// status and one type; add a code here before a handler or service names it.
var errorCodes = []ErrorCode{
	// Generic.
	{CodeInvalidParam, 400, invalid, "The request is malformed or a parameter is invalid; param names the parameter when known."},
	{CodeAuthenticationRequired, 401, authn, "No valid credential was presented."},
	{CodeResourceAccessDenied, 403, authz, "The credential may not access this resource."},
	{CodeResourceNotFound, 404, invalid, "The addressed resource does not exist in this merchant."},
	{CodeResourceConflict, 409, invalid, "The request conflicts with the resource's current state."},
	{CodePaymentFailed, 402, card, "The payment was not made."},
	{CodeRateLimitExceeded, 429, limit, "Too many requests; Retry-After says when to try again."},
	{CodeInternalError, 500, fault, "OpenRails failed; request_id identifies the failure in its logs."},
	{CodeServiceUnavailable, 503, fault, "A dependency is temporarily unavailable; retry."},
	{CodeIdempotencyKeyReused, 409, invalid, "The idempotency key already committed with different terms."},
	{CodeInsufficientCredits, 402, card, "The customer's credit balance does not cover the operation."},
	{CodeInsufficientFunds, 402, card, "The payment instrument lacks funds."},
	{"database_busy", 503, fault, "No database connection is available; retry shortly."},

	// Request shape and transport.
	{CodeUnknownField, 400, invalid, "The JSON body names a field the route does not accept; param is the field."},
	{CodeInvalidQuery, 400, invalid, "A query parameter is malformed or out of range; param is the parameter."},
	{CodeInvalidCursor, 400, invalid, "The cursor is not one this list issued."},
	{CodeInvalidRequestBody, 400, invalid, "The request body could not be read or is not one JSON value."},
	{CodeRequestBodyTooLarge, 413, invalid, "The request body exceeds the deployment's cap."},
	{CodeUnsupportedMediaType, 415, invalid, "The request body is not application/json."},
	{CodeRouteNotFound, 404, invalid, "No route matches the path."},
	{CodeMethodNotAllowed, 405, invalid, "The path exists but not for this method; Allow lists its methods."},
	{"idempotency_key_required", 400, invalid, "The operation needs an Idempotency-Key header."},
	{"csrf_origin_denied", 403, authz, "A cookie-authenticated request came from an origin that is not allowed."},
	{"captcha_required", 403, invalid, "The caller must solve a captcha and resend with its token."},
	{"captcha_invalid", 403, invalid, "The captcha token was rejected."},
	{"card_requires_https", 400, invalid, "Card data is accepted only over HTTPS."},

	// Merchant selection.
	{CodeMerchantSelectorInvalid, 400, invalid, "The OpenRails-Merchant header is malformed, repeated or names no merchant."},
	{CodeMerchantNotFound, 404, invalid, "No active merchant answers to the selector."},
	{CodeMerchantBindingMismatch, 409, invalid, "The selected merchant is not the one the credential, deployment or request is bound to."},
	{CodeMerchantDirectoryUnavailable, 503, fault, "The merchant directory could not be read; retry."},

	// Authentication.
	{CodeCredentialExpired, 401, authn, "The credential has expired."},
	{CodeCredentialRevoked, 401, authn, "The credential or its session was revoked."},
	{CodeCredentialIdentityMismatch, 401, authn, "The credential changed identity during the request."},
	{CodeSenderProofRequired, 401, authn, "A sender-constrained token arrived without its DPoP proof."},
	{CodeServiceCredentialInvalid, 401, authn, "The API key or service token is invalid."},
	{CodeDelegatedTokenInvalid, 401, authn, "The delegated access token is invalid."},
	{CodeDelegatedTokenExpired, 401, authn, "The delegated access token has expired."},
	{CodeDelegatedTokenRevoked, 401, authn, "The delegated access token was revoked."},
	{CodeDelegatedPrincipalInvalid, 401, authn, "The host's delegated principal names no usable merchant or subject."},
	{CodeHostPrincipalInvalid, 401, authn, "The in-process host principal is bound to no merchant."},
	{CodeAuthenticationUnavailable, 503, fault, "The credential could not be verified right now; retry."},
	{CodeDelegatedVerificationUnavailable, 503, fault, "Delegated tokens cannot be verified right now; retry."},

	// Authorization.
	{CodePermissionRequired, 403, authz, "The credential lacks the permission the route requires."},
	{CodeMerchantUnresolved, 403, authz, "The credential names no merchant, or more than one; select one."},
	{CodeHostMerchantMismatch, 403, authz, "The credential's merchant is not the one this host serves."},
	{CodeMerchantContextMismatch, 403, authz, "The authorized merchant is not the one the request resolved."},
	{CodeServiceCredentialMerchantUnresolved, 403, authz, "The service credential's issuer owns no merchant."},
	{CodeServiceCredentialResourceScopeDenied, 403, authz, "The service credential is scoped to other resources."},
	{CodeServiceCredentialCustomerScopeDenied, 403, authz, "The service credential may not act for this customer."},
	{CodeDelegatedMerchantUnresolved, 403, authz, "The delegated token's issuer resolves to no merchant."},
	{CodeInvokerScopedPrincipal, 403, authz, "An invoker-scoped credential spends a customer's balance but may not manage the account."},
	{CodeCatalogOwnerRequired, 403, authz, "The catalog owner could not be established from the credential or selector."},
	{CodeStepUpRequired, 403, authz, "The operation needs a recent sign-in; metadata carries the challenge."},
	{CodeStepUpUnavailable, 403, authz, "The operation needs a recent sign-in and this credential cannot prove one."},
	{CodeAuthorizationUnavailable, 503, fault, "Permissions could not be checked right now; retry."},
	{"customer_action_required", 403, authz, "Only the verified customer may perform this payment action."},
	{"customer_session_required", 403, authz, "The operation needs the customer's interactive session."},
	{"permanent_grant_forbidden", 403, authz, "A grant with no end needs merchant:access:grant-permanent."},

	// Payments and payment methods.
	{CodeCardDeclined, 402, card, "The provider declined the card; metadata.decline_reason says why."},
	{CodePaymentMethodStale, 402, card, "The saved payment method can no longer be charged; collect the card again."},
	{CodePaymentProviderRejected, 502, fault, "The provider refused to process the charge for a gateway or account reason."},
	{CodePaymentMethodRequired, 400, invalid, "The charge names neither a saved payment method nor a new card token."},
	{CodePaymentDuplicateRefused, 409, invalid, "The provider refused an identical charge it had just made; retry after its duplicate window."},
	{CodePaymentMethodSameVault, 409, invalid, "The card is in the provider vault the subscription already bills."},
	{"payment_method_psp_mismatch", 409, invalid, "The payment method is vaulted by another PSP than the subscription's."},
	{"payment_method_not_psp_vaulted", 409, invalid, "The payment method is not held in the provider vault."},
	{"payment_method_not_usable", 409, invalid, "The payment method cannot be charged in its current state."},
	{"payment_method_delete_unsupported", 400, invalid, "This payment method cannot be deleted through OpenRails."},
	{"payment_method_delete_failed", 502, fault, "The provider could not delete the payment method."},
	{"payment_method_update_unsupported", 400, invalid, "This payment method cannot be updated through OpenRails."},
	{"payment_method_update_failed", 502, fault, "The provider could not update the payment method."},
	{"payment_method_update_retry_required", 409, invalid, "The card was not updated; tokenize it again."},
	{"provider_outcome_unknown", 409, fault, "The provider did not confirm the outcome; read the resource before retrying."},
	{"card_not_saved", 409, fault, "The card was not saved; enter it again."},
	{"card_attempts_blocked", 429, limit, "Too many declined card attempts; try again later."},
	{"custodian_capture_unavailable", 503, fault, "The card custodian cannot capture cards right now."},
	{"customer_payment_unsupported", 400, invalid, "Customer-present payment is unsupported for this rail or method."},
	{"invalid_payment_method", 400, invalid, "The payment method is not eligible for this operation."},
	{"payment_idempotency_conflict", 409, invalid, "The idempotency key belongs to another payment request."},
	{"payment_in_progress", 409, invalid, "A payment is already unresolved; read the resource before retrying."},
	{"payment_not_retryable", 409, invalid, "The resource is not payable now."},
	{"payment_not_found", 404, invalid, "The payment or payment operation does not exist."},
	{"refund_rail_unavailable", 409, invalid, "The payment's rail cannot accept a refund right now."},
	{"refund_unsupported", 400, invalid, "The payment's rail has no automatic refund."},

	// Checkout.
	{"checkout_session_not_found", 404, invalid, "The checkout session does not exist."},
	{"checkout_session_expired", 410, invalid, "The checkout session expired."},
	{"checkout_session_unavailable", 403, authz, "The checkout session is not available to this caller."},
	{"checkout_session_closed", 409, invalid, "The checkout session already completed or was canceled."},
	{"checkout_payment_in_progress", 409, invalid, "A payment on this checkout session is already being processed."},
	{"checkout_request_invalid", 422, invalid, "The checkout request is invalid."},
	{"checkout_offer_unavailable", 422, invalid, "The purchase is not available."},

	// Subscriptions and tier changes.
	{"subscription_not_found", 404, invalid, "The subscription does not exist."},
	{"subscription_not_active", 409, invalid, "The subscription is not active."},
	{"cancel_unsupported_on_rail", 400, invalid, "This rail has no cancel operation."},
	{"solana_cancel_needs_wallet_signature", 400, invalid, "Cancelling a Solana subscription needs the subscriber's wallet signature."},
	{CodeProviderCancelHeld, 409, invalid, "Cancelling needs a destructive provider action that is not armed for this merchant."},
	{"rebill_terms_committed", 409, invalid, "An accepted recurring payment owns the pending price terms."},
	{CodeTierChangeInFlight, 409, invalid, "Another unresolved tier change owns the subscription; metadata.operation_id names it."},
	{CodeTierChangeRefused, 409, invalid, "The tier change was refused and not executed."},
	{CodeTierChangeIdempotencyConflict, 409, invalid, "The Idempotency-Key already names a different tier change."},
	{CodeTierChangeIdempotencyKeyRequired, 400, invalid, "A tier change needs an Idempotency-Key."},
	{CodeTierChangeCycleUnknown, 422, invalid, "The target price has no positive billing cycle."},
	{CodeTierChangePeriodUnknown, 422, invalid, "The subscription has no valid current period."},
	{CodeTierChangeCreditExceedsPrice, 409, invalid, "The current plan's unused value exceeds the target price; change at period end."},
	{CodeTierChangeRenewalDue, 409, invalid, "The current period ended or its renewal is unresolved; the renewal settles first."},
	{CodeTierChangeAlreadyScheduled, 409, invalid, "A different period-end change is already scheduled."},
	{CodeTierChangeCadenceUnsupported, 409, invalid, "A provider-billed subscription can change only to a price of the same cadence."},
	{CodeTierChangeRequiresLinkedPlan, 409, invalid, "The target price has no linked provider plan of the same amount and cycle."},
	{"reprice_not_found", 404, invalid, "The reprice does not exist."},
	{"reprice_price_key_not_found", 404, invalid, "The reprice names a price key that does not exist."},
	{"reprice_target_price_not_found", 404, invalid, "The reprice's target price does not exist."},
	{"reprice_already_scheduled", 409, invalid, "The subscription already has a scheduled reprice."},
	{"reprice_not_scheduled", 409, invalid, "The reprice is no longer scheduled."},
	{"reprice_cross_currency", 422, invalid, "The target price must be in the same currency."},
	{"reprice_cross_product", 422, invalid, "The target price must be on the same product."},
	{"reprice_inactive_price", 422, invalid, "The target price must be active."},
	{"reprice_notice_window_violation", 422, invalid, "effective_at is inside the merchant's notice window for a price increase."},
	{"provider_cutover_unavailable", 503, fault, "Provider cutover is not available in this deployment."},
	{"provider_cutover_unqualified", 409, invalid, "Both PSPs need explicit cutover qualification."},
	{"provider_cutover_conflict", 409, invalid, "The cutover conflicts with the subscription's current state."},

	// Invoices and collection.
	{CodeInvoiceActionNotAllowed, 409, invalid, "The invoice's status does not allow this action."},
	{CodeInvoiceNotRetryable, 409, invalid, "The invoice cannot be collected again."},
	{CodeInvoiceRetryInProgress, 409, invalid, "A collection attempt on this invoice is unresolved."},
	{CodeInvoiceRetryOutcomeUnknown, 409, invalid, "The last collection attempt's outcome is unknown."},
	{CodeInvoiceRetryIdempotencyConflict, 409, invalid, "The idempotency key names a different collection attempt."},
	{CodeInvoicePaymentReferenceUsed, 409, invalid, "The payment reference is already recorded."},
	{CodeInvoicePaymentExceedsDue, 409, invalid, "The recorded payment exceeds the amount due."},
	{CodeInvoicePaymentInvalid, 400, invalid, "The recorded payment is invalid."},
	{CodeCollectionPaymentMethodRequired, 400, invalid, "Collection needs a payment method for the invoice's currency."},
	{CodeCollectionPaymentMethodInvalid, 400, invalid, "The collection payment method cannot pay this invoice."},

	// Credits, admission and provider obligations.
	{"credit_grant_not_found", 404, invalid, "The credit grant does not exist."},
	{"credit_grant_held", 409, invalid, "Active holds need the grant's remaining credit."},
	{"credit_grant_unavailable", 409, invalid, "The credit grant expired, ended or has no remaining credit."},
	{"admission_not_found", 404, invalid, "No admission was made under this request id."},
	{"admission_captured", 409, invalid, "The admission was captured; it can no longer be released."},
	{"hold_not_found", 404, invalid, "The admission holds nothing open: it was captured, released or lapsed."},
	{"spend_delegation_not_found", 404, invalid, "The spend delegation does not exist."},
	{"currency_unsupported", 400, invalid, "The currency is not in OpenRails' registry."},
	{"operation_authorization_not_found", 404, invalid, "The operation authorization does not exist."},
	{"operation_authorization_conflict", 409, invalid, "The operation id was reused with a changed term; param names it."},
	{"operation_authorization_not_open", 409, invalid, "The operation authorization is no longer open."},
	{"operation_authorization_has_billing_evidence", 409, invalid, "The operation authorization already carries provider billing evidence."},
	{"provider_billing_qualification_not_found", 404, invalid, "The operation has no provider billing qualification."},
	{"provider_billing_observation_conflict", 409, invalid, "The provider billing evidence conflicts with a recorded observation; param names it."},
	{"provider_billing_qualification_refused", 409, invalid, "The provider billing evidence was refused."},
	{"invalid_settlement_status_request", 400, invalid, "A settlement status read needs a customer and a price."},

	// Customers.
	{"customer_not_found", 404, invalid, "The customer does not exist."},
	{"invalid_customer_id", 400, invalid, "The customer id is missing or malformed."},
	{"billing_policy_not_found", 404, invalid, "The billing policy does not exist."},
	{"host_event_not_found", 404, invalid, "The host event does not exist."},
	{"invalid_host_event_request", 400, invalid, "The host event request is invalid."},

	// Catalog and metering.
	{"product_not_found", 404, invalid, "The product does not exist."},
	{"price_not_found", 404, invalid, "The price does not exist."},
	{"price_key_not_found", 404, invalid, "No price holds this key."},
	{"price_key_cadence_conflict", 409, invalid, "The product's default price key is held by a price on another cadence."},
	{"price_not_sellable", 400, invalid, "No PSP can sell this price."},
	{"trial_unsupported_on_rail", 400, invalid, "This rail cannot run a trial first phase."},
	{"product_tier_group_conflict", 409, invalid, "A customer holds live subscriptions to more than one product of the tier group."},
	{"product_tier_group_in_use", 409, invalid, "The tier group cannot change while a subscription has a plan change in flight."},
	{"catalog_not_found", 404, invalid, "The catalog does not exist."},
	{"catalog_owner_forbidden", 403, authz, "A catalog owner cannot change merchant-wide catalog settings."},
	{"catalog_scope_mismatch", 403, authz, "The catalog scope does not match the authorized merchant and catalog."},
	{"catalog_updates_disabled", 403, invalid, "Catalog updates over HTTP are disabled in this deployment."},
	{"catalog_declared", 405, invalid, "The catalog is declared by the host; change the declaration and restart."},
	{"catalog_application_conflict", 409, invalid, "The application id already committed with different content."},
	{"catalog_revision_conflict", 409, invalid, "The catalog changed during the application; retry."},
	{"usage_meter_not_found", 404, invalid, "The usage meter does not exist."},
	{"default_rate_card_not_found", 404, invalid, "The meter has no default rate card."},
	{"rate_card_product_not_found", 404, invalid, "The rate card names a product that does not exist."},
	{"allowance_meter_not_found", 404, invalid, "The rate card's allowance meter does not exist."},
	{"usage_rate_card_invalid", 400, invalid, "The usage rate card is invalid."},
	{"usage_meter_invalid", 400, invalid, "The meter definition is invalid."},
	{"meter_in_use", 409, invalid, "The meter is referenced and cannot change this way."},
	{"meter_rate_card_conflict", 409, invalid, "The meter and its rate card disagree."},
	{"allowance_source_invalid", 409, invalid, "The allowance source cannot back this rate card."},
	{"allowance_source_in_use", 409, invalid, "The allowance source is in use."},
	{"default_rate_card_required", 409, invalid, "The meter needs a default rate card."},
	{"rate_card_has_overrides", 409, invalid, "The rate card still has customer overrides."},
	{"rate_card_currency_mismatch", 409, invalid, "The rate card's currency does not match."},
	{"purchase_review_resolved", 409, invalid, "The purchase review was already resolved."},

	// Merchant configuration and PSPs.
	{"payment_provider_not_found", 404, invalid, "The merchant has no PSP configured for this rail."},
	{"payment_provider_credentials_rejected", 400, invalid, "The payment provider rejected the credentials."},
	{"provider_account_last_active", 409, invalid, "The PSP is the rail's last active account and still has live subscriptions."},
	{"provider_accounts_ambiguous", 409, invalid, "The rail has several active PSPs; address one."},
	{"psp_claim_requires_proof", 403, authz, "Claiming a provider account needs credentials that prove control of it."},
	{"credential_custody_transition_required", 409, invalid, "Credential custody differs from the published backend."},
	{"credential_operation_conflict", 409, invalid, "The credential operation conflicts with the published revision."},
	{"credential_source_read_only", 405, invalid, "The provider credential source has no writable custody."},
	{"credential_store_read_only", 403, authz, "The credential store is read-only."},
	{"invalid_psp_reference", 400, invalid, "The PSP reference is invalid."},
	{"merchant_configuration_application_conflict", 409, invalid, "The application id already committed with different content."},
	{"merchant_configuration_revision_conflict", 409, invalid, "The merchant configuration changed; read its revision before applying."},
	{"api_host_requires_proof", 409, invalid, "A new api_host must be claimed and proven before configuration names it."},
	{"invalid_api_host", 400, invalid, "api_host must be a bare lowercase domain name."},
	{"api_host_reserved", 400, invalid, "The api_host serves this deployment."},
	{"api_host_taken", 409, invalid, "The api_host is assigned to another merchant."},
	{"api_host_claim_missing", 409, invalid, "No api_host has been claimed."},
	{"api_host_unproven", 409, invalid, "The api_host's DNS proof was not found."},
	{"webhook_invalid", 400, invalid, "The outbound webhook is invalid."},
	{"webhook_account_mismatch", 400, invalid, "The webhook's account does not match its payload."},
	{"metrics_query_invalid", 400, invalid, "The metrics query is invalid; metadata.errors lists why."},
	{"dashboard_invalid", 400, invalid, "The dashboard is invalid; metadata.errors lists why."},
	{"widget_generation_invalid", 422, invalid, "The model could not produce a valid query for the prompt."},

	// Billing import and archive.
	{"as_of_required", 400, invalid, "The import needs as_of, its RFC 3339 evidence horizon."},
	{"billing_archive_unavailable", 500, fault, "The billing archive operation failed."},
	{"billing_archive_invalid_artifact", 400, invalid, "The billing archive is invalid, incomplete or unsupported."},
	{"billing_archive_merchant_mismatch", 409, invalid, "The archive's merchant is not the destination's."},
	{"billing_archive_not_empty", 409, invalid, "The destination's billing state must be empty."},
	{"billing_archive_unsupported_state", 409, invalid, "The merchant has state that cannot safely be moved."},
	{"billing_archive_integrity", 422, invalid, "The billing archive failed integrity validation."},

	// Standalone control plane.
	{"invalid_name", 400, invalid, "The name is missing or too long."},
	{"invalid_email", 400, invalid, "The email is missing or malformed."},
	{"invalid_user", 400, invalid, "The user id is missing or malformed."},
	{"unknown_role", 400, invalid, "The role is not one this merchant defines."},
	{"role_escalation", 403, authz, "The grant exceeds the caller's own authority."},
	{"credentials_manage_required", 403, authz, "The account lacks credential-management authority on this merchant."},
	{"members_manage_required", 403, authz, "The account lacks team-management authority on this merchant."},
	{"last_owner", 400, invalid, "A merchant must keep at least one owner."},
	{"invites_disabled", 409, invalid, "The email has no verified account and invitations by registration are disabled."},
	{"name_taken", 409, invalid, "The merchant name is taken."},
	{"name_reserved", 409, invalid, "The merchant name is reserved."},
	{"renames_disabled", 403, invalid, "Merchant renames are disabled."},
	{"rename_too_soon", 429, invalid, "The merchant was renamed too recently."},
	{"email_unverified", 403, authz, "Creating a merchant needs a verified email."},
	{"creation_refused", 403, authz, "Merchant creation was refused."},
	{CodeMerchantCreationPaymentMethodRequired, 402, card, "Creating another merchant needs a payment method on file."},
}

var errorCodeIndex = func() map[string]ErrorCode {
	index := make(map[string]ErrorCode, len(errorCodes))
	for _, c := range errorCodes {
		if _, dup := index[c.Code]; dup {
			panic("billing: error code registered twice: " + c.Code)
		}
		if c.Status < 400 || c.Status > 599 || http.StatusText(c.Status) == "" || c.Type == "" || c.Meaning == "" {
			panic("billing: incomplete error code: " + c.Code)
		}
		index[c.Code] = c
	}
	return index
}()

// ErrorCodes lists every registered error code, sorted by code.
func ErrorCodes() []ErrorCode {
	out := append([]ErrorCode(nil), errorCodes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// LookupErrorCode returns the registration of a wire code.
func LookupErrorCode(code string) (ErrorCode, bool) {
	c, ok := errorCodeIndex[code]
	return c, ok
}
