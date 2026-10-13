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
	// CodeProviderWritesDisabled: the merchant's provider writes are off
	// (provider_write_mode, or the merchant's write posture); the message
	// names why.
	CodeProviderWritesDisabled = "provider_writes_disabled"
	CodeIdempotencyKeyReused   = "idempotency_key_reused"
	CodeIdempotencyKeyInUse    = "idempotency_key_in_use"
	CodeInsufficientCredits    = "insufficient_credits"
	CodeInsufficientFunds      = "insufficient_funds"
	// CodeUsageNotPriced: a priced meter cannot price a usage event.
	CodeUsageNotPriced = "usage_not_priced"
	// Orders.
	CodeAlreadyOwned             = "already_owned"
	CodeEntitlementRequired      = "entitlement_required"
	CodeOrderLineUnavailable     = "order_line_unavailable"
	CodeQuantityNotAllowed       = "quantity_not_allowed"
	CodeOrderTotalChanged        = "order_total_changed"
	CodeOrderPaymentInProgress   = "order_payment_in_progress"
	CodeOrderNotPayable          = "order_not_payable"
	CodeOrderNotCancelable       = "order_not_cancelable"
	CodeOrderActionNotAllowed    = "order_action_not_allowed"
	CodePaymentOptionUnavailable = "payment_option_unavailable"
	// Payments recorded outside OpenRails.
	CodePaymentExceedsDue     = "payment_exceeds_due"
	CodeOrderHasRecurringLine = "order_has_recurring_line"
	// CodeModelUnavailable: the language model behind an ask or generate
	// route did not answer.
	CodeModelUnavailable = "model_unavailable"
)

// Refusals of buying a product again while a canceled subscription to it is
// still paid; nothing is charged.
const (
	// CodeSubscriptionResumable: resume the canceled subscription instead.
	CodeSubscriptionResumable = "subscription_resumable"
	// CodeSubscriptionPaidThrough: buy again once its paid period ends.
	CodeSubscriptionPaidThrough = "subscription_paid_through"
)

// Scheduled changes and price migrations.
const (
	CodeScheduledChangeExists       = "scheduled_change_exists"
	CodePriceMigrationNotFound      = "price_migration_not_found"
	CodePriceChangeCurrencyMismatch = "price_change_currency_mismatch"
	CodePriceChangeTargetArchived   = "price_change_target_archived"
	CodePriceIncreaseNoticeTooShort = "price_increase_notice_too_short"
)

// CodeCatalogBenefitOverlap: two recurring products would grant one
// entitlement outside a shared tier group.
const CodeCatalogBenefitOverlap = "catalog_benefit_overlap"

// CodeRevisionMismatch: an edit's expected_revision is not the object's
// current revision; metadata.revision is the current one.
const CodeRevisionMismatch = "revision_mismatch"

// CodeProductInUse: a record refers to the product or one of its prices (a
// purchase, subscription, grant, access, pay URL, migration, provider write,
// PSP link or rate card), so it is never deleted; archive it instead.
const CodeProductInUse = "product_in_use"

// Request-shape codes: the request never reached its operation.
const (
	// CodeUnknownField: the JSON body names a field the route does not
	// accept; param is the field.
	CodeUnknownField = "unknown_field"
	// CodeInvalidQuery: a query parameter is malformed or out of range;
	// param is the parameter.
	CodeInvalidQuery = "invalid_query"
	// CodeInvalidCursor: the list cursor is not one this list issued.
	CodeInvalidCursor        = "invalid_cursor"
	CodeUnsupportedMediaType = "unsupported_media_type"
	CodeRouteNotFound        = "route_not_found"
	CodeMethodNotAllowed     = "method_not_allowed"
	// CodeMerchantMismatch: the credential's merchant differs from the
	// request's expected merchant; no operation was executed.
	CodeMerchantMismatch = "merchant_mismatch"
)

// Customer link codes (openrails.dev).
const (
	CodeNetworkLinkChallengeInvalid = "network_link_challenge_invalid"
	CodeNetworkLinkConflict         = "network_link_conflict"
	CodeNetworkAccountLinked        = "network_account_linked"
	CodeNetworkTermsRequired        = "network_terms_required"
	CodeNetworkLinkInUse            = "network_link_in_use"
)

// Authentication and authorization codes: why a credential was refused.
const (
	CodeCredentialExpired                    = "credential_expired"
	CodeCredentialRevoked                    = "credential_revoked"
	CodeServiceCredentialCustomerScopeDenied = "service_credential_customer_scope_denied"
	CodeHostPrincipalInvalid                 = "host_principal_invalid"
	CodePermissionRequired                   = "permission_required"
	CodeMerchantUnresolved                   = "merchant_unresolved"
	CodeCustomerIssuerMismatch               = "customer_issuer_mismatch"
	CodeInvokerScopedPrincipal               = "invoker_scoped_principal"
	CodeApplicationRequired                  = "application_required"
	CodeStepUpRequired                       = "step_up_required"
	CodeStepUpUnavailable                    = "step_up_unavailable"
	CodeAuthenticationUnavailable            = "authentication_unavailable"
	CodeAuthorizationUnavailable             = "authorization_unavailable"
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
	{CodeProviderWritesDisabled, 503, fault, "Provider writes are off for the merchant, so it takes no payment; the message names why (provider_write_mode or the merchant's write posture)."},
	{CodeIdempotencyKeyReused, 422, invalid, "The idempotency key already committed with different terms."},
	{CodeIdempotencyKeyInUse, 409, invalid, "The request first sent with this Idempotency-Key is still running; retry once it finishes."},
	{CodeInsufficientCredits, 402, card, "The customer's credit balance does not cover the operation."},
	{CodeInsufficientFunds, 402, card, "The payment instrument lacks funds."},
	{"database_busy", 503, fault, "No database connection is available; retry shortly."},
	{CodeModelUnavailable, 502, fault, "The language model did not answer; retry, or ask a narrower question."},

	// Request shape and transport.
	{CodeUnknownField, 400, invalid, "The JSON body names a field the route does not accept; param is the field."},
	{CodeInvalidQuery, 400, invalid, "A query parameter is malformed or out of range; param is the parameter."},
	{CodeInvalidCursor, 400, invalid, "The cursor is not one this list issued."},
	{CodeInvalidRequestBody, 400, invalid, "The request body could not be read or is not one JSON value."},
	{CodeRequestBodyTooLarge, 413, invalid, "The request body exceeds the deployment's cap."},
	{CodeUnsupportedMediaType, 415, invalid, "The request body is not application/json."},
	{CodeRouteNotFound, 404, invalid, "No route matches the path."},
	{CodeMethodNotAllowed, 405, invalid, "The path exists but not for this method; Allow lists its methods."},
	{CodeMerchantMismatch, 412, invalid, "The credential is bound to a different merchant than OpenRails-Expected-Merchant; the operation was not executed."},
	{"idempotency_key_required", 400, invalid, "The operation needs an Idempotency-Key header."},
	{"csrf_origin_denied", 403, authz, "A cookie-authenticated request came from an origin that is not allowed."},
	{"captcha_required", 403, invalid, "The caller must solve a captcha and resend with its token."},
	{"captcha_invalid", 403, invalid, "The captcha token was rejected."},
	{"card_requires_https", 400, invalid, "Card data is accepted only over HTTPS."},

	// Authentication.
	{CodeCredentialExpired, 401, authn, "The credential has expired."},
	{CodeCredentialRevoked, 401, authn, "The credential or its session was revoked."},
	{CodeHostPrincipalInvalid, 401, authn, "The in-process host principal is bound to no merchant, or names an invalid invoker."},
	{CodeAuthenticationUnavailable, 503, fault, "The credential could not be verified right now; retry."},
	{CodeStepUpRequired, 401, authn, "The operation needs a recent sign-in (RFC 9470: WWW-Authenticate insufficient_user_authentication with max_age); metadata carries the provider's challenge."},

	// Authorization.
	{CodePermissionRequired, 403, authz, "The credential lacks the permission the route requires."},
	{CodeMerchantUnresolved, 403, authz, "The credential is bound to no merchant here."},
	{CodeCustomerIssuerMismatch, 403, authz, "The customer belongs to another issuer's user with the same subject."},
	{CodeServiceCredentialCustomerScopeDenied, 403, authz, "The service credential may not act for this customer."},
	{CodeInvokerScopedPrincipal, 403, authz, "An invoker-scoped credential spends a customer's balance but may not manage the account."},
	{CodeApplicationRequired, 403, authz, "The route is your backend's: it takes an application's credential, never a person's."},
	{CodeStepUpUnavailable, 403, authz, "The operation needs a recent sign-in and this credential cannot prove one."},
	{CodeAuthorizationUnavailable, 503, fault, "Permissions could not be checked right now; retry."},
	{"customer_action_required", 403, authz, "Only the customer may take this action, through their own step."},
	{"customer_session_required", 403, authz, "The operation needs the customer's interactive session."},

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
	{"provider_outcome_unknown", 409, fault, "The provider did not confirm the outcome; read the resource before retrying."},
	{"card_not_saved", 409, fault, "The card was not saved; enter it again."},
	{"card_attempts_blocked", 429, limit, "Too many declined card attempts; try again later."},
	{"custodian_capture_unavailable", 503, fault, "The card custodian cannot capture cards right now."},
	{"customer_payment_unsupported", 400, invalid, "Customer-present payment is unsupported for this rail or method."},
	{"invalid_payment_method", 400, invalid, "The payment method is not eligible for this operation."},
	{"payment_idempotency_conflict", 409, invalid, "The idempotency key belongs to another payment request."},
	{"payment_in_progress", 409, invalid, "A payment is already unresolved; read the resource before retrying."},
	{"payment_not_retryable", 409, invalid, "The resource is not payable now."},
	{"payment_not_found", 404, invalid, "The payment does not exist."},
	{"refund_rail_unavailable", 409, invalid, "The payment's rail cannot accept a refund right now."},
	{"refund_unsupported", 400, invalid, "The payment's rail has no automatic refund."},
	{"payment_not_refundable", 400, invalid, "The payment is not a completed rail charge, or the amount exceeds what remains refundable."},
	{"refund_failed", 502, fault, "The provider refused the refund."},

	// Pay URLs.
	{"pay_url_not_found", 404, invalid, "The pay URL is unknown or no longer valid."},
	{"pay_url_expired", 410, invalid, "The pay URL expired; the merchant creates a new order."},
	{"pay_url_unavailable", 400, invalid, "This deployment serves no payment origin, or the order cannot take a pay URL."},
	{"pay_url_attempts_exhausted", 409, invalid, "The pay URL took its most card attempts; the merchant creates a new order."},
	{"embed_origins_required", 400, invalid, "An embedded pay URL needs the origins that may frame it: the merchant's settings payment_page.embed_origins."},
	{"order_attempt_closed", 409, invalid, "The order attempt already completed or was canceled."},
	{"order_attempt_expired", 410, invalid, "The order attempt expired before it was paid."},

	// Orders.
	{CodeAlreadyOwned, 409, invalid, "The customer already holds what a line buys; metadata.owned_by names the holder and metadata.hint says change or resume."},
	{CodeEntitlementRequired, 403, invalid, "A line's product sells only to customers who already hold its required entitlements; param names the line and metadata.missing the keys the customer lacks."},
	{CodeOrderLineUnavailable, 422, invalid, "A line cannot be bought; param names it and metadata.code says why."},
	{CodeQuantityNotAllowed, 422, invalid, "A quantity was sent for a price that is not sold per seat."},
	{CodeOrderTotalChanged, 409, invalid, "The order's total is not expected_total; preview it again."},
	{CodeOrderPaymentInProgress, 409, invalid, "A payment on this order is unresolved; read the order."},
	{CodeOrderNotPayable, 409, invalid, "The order takes no payment: it is paid, canceled or expired."},
	{CodeOrderNotCancelable, 409, invalid, "Only an open order, or one awaiting the customer's action, can be canceled."},
	{CodeOrderActionNotAllowed, 409, invalid, "The order's origin or status does not allow this action."},
	{CodePaymentOptionUnavailable, 422, invalid, "No PSP that can take the order's lines accepts this payment."},
	{CodeOrderHasRecurringLine, 409, invalid, "An order with a recurring line is paid by the customer, whose card its renewals charge; it cannot be recorded as paid."},
	{CodePaymentExceedsDue, 409, invalid, "The recorded payment exceeds what the usage order has due, or is not the order's total."},

	// Subscriptions and tier changes.
	{CodeCatalogBenefitOverlap, 409, invalid, "Two recurring products would grant one entitlement outside a shared tier group; put them in one tier group."},
	{"subscription_not_found", 404, invalid, "The subscription does not exist."},
	{"subscription_not_active", 409, invalid, "The subscription is not active."},
	{"cancel_unsupported_on_rail", 400, invalid, "This rail has no cancel operation."},
	{CodeProviderCancelHeld, 409, invalid, "Cancelling needs a destructive provider action that is not armed for this merchant."},
	{CodeSubscriptionResumable, 409, invalid, "A canceled subscription to this product is still paid and can be resumed; resume it instead of buying again."},
	{CodeSubscriptionPaidThrough, 409, invalid, "A canceled subscription to this product is still paid; buy again once its paid period ends."},
	{"rebill_terms_committed", 409, invalid, "An accepted recurring payment owns the pending price terms."},
	{CodeSubscriptionChangeInFlight, 409, invalid, "Another unresolved change owns the subscription: read it back with its Idempotency-Key, or confirm it."},
	{CodeSubscriptionChangeRefused, 409, invalid, "The change was refused and not executed."},
	{CodeSubscriptionChangeIdempotencyConflict, 409, invalid, "The Idempotency-Key already names a different change."},
	{CodeSubscriptionChangeIdempotencyKeyRequired, 400, invalid, "A subscription change needs an Idempotency-Key."},
	{CodeSubscriptionChangeCycleUnknown, 422, invalid, "The target price has no positive billing cycle."},
	{CodeSubscriptionChangePeriodUnknown, 422, invalid, "The subscription has no valid current period."},
	{CodeSubscriptionChangeCreditExceedsPrice, 409, invalid, "The current plan's unused value exceeds the target price; change at period end."},
	{CodeSubscriptionChangeRenewalDue, 409, invalid, "The current period ended or its renewal is unresolved; the renewal settles first."},
	{CodeSubscriptionChangeAlreadyScheduled, 409, invalid, "A different period-end change is already scheduled."},
	{CodeSubscriptionChangeCadenceUnsupported, 409, invalid, "A provider-billed subscription can change only to a price of the same cadence."},
	{CodeSubscriptionChangeRequiresLinkedPlan, 409, invalid, "The target price has no plan on the subscription's PSP that this change can use."},
	{CodeSubscriptionChangeTargetInactive, 422, invalid, "The target price or its product is archived."},
	{CodeSubscriptionChangeUnsupportedOnRail, 400, invalid, "The subscription's rail cannot make this change."},
	{CodeSubscriptionChangeProviderConflict, 409, invalid, "The provider's copy of the subscription is missing or differs; reconcile it first."},
	{CodeStoredCredentialRequired, 409, invalid, "The card has no active agreement for a merchant-initiated charge; the customer makes this change."},
	{"customer_email_required", 400, invalid, "The rail needs the customer's verified email and username."},
	{"solana_transaction_refused", 400, invalid, "The wallet transaction could not be prepared or confirmed; the message says why."},
	{"solana_rpc_unavailable", 502, fault, "The Solana RPC endpoints did not answer; retry."},
	{CodeScheduledChangeExists, 409, invalid, "The subscription already has a scheduled change."},
	{CodePriceMigrationNotFound, 404, invalid, "The price migration does not exist."},
	{CodePriceChangeCurrencyMismatch, 422, invalid, "The target price must be in the subscription's currency."},
	{CodePriceChangeTargetArchived, 422, invalid, "The target price is archived."},
	{CodePriceIncreaseNoticeTooShort, 422, invalid, "effective_at is inside the merchant's notice window for a price increase."},

	// Collection.
	{CodeDefaultPaymentMethodRequired, 400, invalid, "The customer has no default card for the currency."},
	{CodeDefaultPaymentMethodInvalid, 400, invalid, "The card cannot be the customer's default for the currency."},

	// Credits and admission.
	{"credit_grant_not_found", 404, invalid, "The credit grant does not exist."},
	{"credit_grant_held", 409, invalid, "Active holds need the grant's remaining credit."},
	{"credit_grant_unavailable", 409, invalid, "The credit grant expired, ended or has no remaining credit."},
	{"admission_not_found", 404, invalid, "No admission was made under this request id."},
	{"admission_captured", 409, invalid, "The admission was captured; it can no longer be released."},
	{"hold_not_found", 404, invalid, "The admission holds nothing open: it was captured, released or lapsed."},
	{"currency_unsupported", 400, invalid, "The currency is not in OpenRails' registry."},
	{"running_charge_not_found", 404, invalid, "No running charge was started under this reference."},
	{"invalid_settlement_status_request", 400, invalid, "A settlement status read needs a customer and a price."},

	// Customers.
	{"customer_not_found", 404, invalid, "The customer does not exist."},
	{"invalid_customer_id", 400, invalid, "The customer id is missing or malformed."},
	{"billing_policy_not_found", 404, invalid, "The billing policy does not exist."},
	{"host_event_not_found", 404, invalid, "The host event does not exist."},
	{"invalid_host_event_request", 400, invalid, "The host event request is invalid."},

	// Catalog and metering.
	{"product_not_found", 404, invalid, "The product does not exist."},
	{CodeProductInUse, 409, invalid, "Records refer to the product; archive it instead."},
	{"price_not_found", 404, invalid, "The price does not exist."},
	{"price_key_not_found", 404, invalid, "No price holds this key."},
	{"price_key_cadence_conflict", 409, invalid, "The product's default price key is held by a price on another cadence."},
	{"price_not_sellable", 400, invalid, "No PSP can sell this price."},
	{"trial_unsupported_on_rail", 400, invalid, "This rail cannot run a trial first phase."},
	{"product_tier_group_conflict", 409, invalid, "A customer holds live subscriptions to more than one product of the tier group."},
	{"product_tier_group_in_use", 409, invalid, "The tier group cannot change while a subscription has a plan change in flight."},
	{CodeRevisionMismatch, 409, invalid, "The object changed since the revision the edit expected; metadata.revision is the current one."},
	{"catalog_revision_conflict", 409, invalid, "The catalog changed during the application; retry."},
	{"usage_meter_not_found", 404, invalid, "The usage meter does not exist."},
	{"default_rate_card_not_found", 404, invalid, "The meter has no default rate card."},
	{"rate_card_product_not_found", 404, invalid, "The rate card names a product that does not exist."},
	{"usage_rate_card_invalid", 400, invalid, "The usage rate card is invalid."},
	{"usage_meter_invalid", 400, invalid, "The meter definition is invalid."},
	{CodeUsageNotPriced, 422, invalid, "A priced meter cannot price the usage (no matrix cell for its value, or no rate card in its currency); param names the field."},
	{"meter_in_use", 409, invalid, "The meter is referenced and cannot change this way."},
	{"meter_rate_card_conflict", 409, invalid, "The meter and its rate card disagree."},
	{"purchase_review_resolved", 409, invalid, "The purchase review was already resolved."},

	// Findings.
	{"finding_not_actionable", 422, invalid, "The finding carries no recommendation to approve; ignore it or fix it out of band."},
	{"finding_action_failed", 502, fault, "Running the finding's recommendation failed; the finding stays open with the error in its notes."},
	{CodeFindingRequiresMerchantConfig, 409, invalid, "The finding is a pull.* proposal: it is approved or dismissed as a proposal, with merchant config."},
	{CodeDestructiveActionsHeld, 409, invalid, "The kill switch holds changes that end access or write to a provider; the proposal stays open."},
	{CodeProviderWritesHeld, 409, invalid, "The merchant's readonly write posture holds changes that move money or write to a provider; the proposal stays open."},

	// Merchant configuration and PSPs.
	{"psp_not_found", 404, invalid, "The PSP does not exist."},
	{"psp_exists", 409, invalid, "The account is already a PSP, of this merchant or another; update it instead."},
	{"psp_key_taken", 409, invalid, "Another live PSP holds the key."},
	{"psp_credentials_rejected", 400, invalid, "The provider rejected the credentials, or answered that they are not in the PSP's declared environment."},
	{"psp_environment_mixed", 409, invalid, "The PSP's environment disagrees with its merchant: a sandbox merchant's PSPs move no money, a live merchant's real money."},
	{"psp_last_active", 409, invalid, "The PSP is the last active one on its rail; pass allow_last to archive it."},
	{"psp_claim_requires_proof", 403, authz, "Claiming a provider account needs credentials that prove control of it."},
	{"invalid_psp_reference", 400, invalid, "The PSP reference is invalid."},
	{"merchant_config_read_only", 409, invalid, "The merchant's configuration is read from a file; change the file."},
	{"card_custody_reserved", 400, invalid, "settings.card_custody hosted is reserved for a private vault the platform will host; choose platform or merchant."},
	{"platform_vault_unavailable", 409, invalid, "The deployment has no platform vault for its posture, or its identity for this merchant names another vault tenant; keep card_custody merchant."},
	{"platform_vault_unreachable", 409, invalid, "A card PSP routing offers cannot charge cards from the platform vault (Stripe, or card_entry server); archive it or keep card_custody merchant."},
	{"webhook_invalid", 400, invalid, "The outbound webhook is invalid."},
	{"webhook_account_mismatch", 400, invalid, "The webhook's account does not match its payload."},
	{"metrics_query_invalid", 400, invalid, "The metrics query is invalid; metadata.errors lists why."},
	{"dashboard_invalid", 400, invalid, "The dashboard is invalid; metadata.errors lists why."},

	// Billing import and archive.
	{"as_of_required", 400, invalid, "The import needs as_of, its RFC 3339 evidence horizon."},
	{"billing_archive_unavailable", 500, fault, "The billing archive operation failed."},
	{"billing_archive_invalid_artifact", 400, invalid, "The billing archive is invalid, incomplete or unsupported."},
	{"billing_archive_merchant_mismatch", 409, invalid, "The archive's merchant is not the destination's."},
	{"billing_archive_not_empty", 409, invalid, "The destination's billing state must be empty."},
	{"billing_archive_unsupported_state", 409, invalid, "The merchant has state that cannot safely be moved."},
	{"billing_archive_platform_cards", 409, invalid, "The merchant holds cards in the platform vault; export or remove them first."},
	{"billing_archive_integrity", 422, invalid, "The billing archive failed integrity validation."},

	// Customer links (openrails.dev).
	{CodeNetworkLinkChallengeInvalid, 400, invalid, "The link challenge is unknown, expired or spent, or the ID token does not answer it: another client, nonce or merchant, a sign-in older than the challenge, or no openrails:link consent; ask for a new challenge."},
	{CodeNetworkLinkConflict, 409, invalid, "The customer is linked to another openrails.dev account; sign in with that one."},
	{CodeNetworkAccountLinked, 409, invalid, "The openrails.dev account is linked to another customer of this merchant."},
	{CodeNetworkTermsRequired, 403, authz, "The openrails.dev account has not accepted the network terms."},
	{CodeNetworkLinkInUse, 409, invalid, "The customer's subscriptions at this merchant still charge its cards; cancel them or move them to another card first."},
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
