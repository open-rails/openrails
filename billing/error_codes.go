package billing

import (
	"net/http"
	"sort"
)

// ErrorType is a refusal's category: ErrorDetails.Type.
type ErrorType string

// Error envelope types.
const (
	ErrorTypeInvalidRequest ErrorType = "invalid_request_error"
	ErrorTypeAuthentication ErrorType = "authentication_error"
	ErrorTypeAuthorization  ErrorType = "authorization_error"
	ErrorTypeCard           ErrorType = "card_error"
	ErrorTypeRateLimit      ErrorType = "rate_limit_error"
	ErrorTypeAPI            ErrorType = "api_error"
)

// ErrorCode is a wire error code: ErrorDetails.Code. Each one is registered
// (ErrorCodes) and has a constant; ErrorCodeFrom reads one from an error.
type ErrorCode string

// ErrorCodeInfo is a code's registration: the HTTP status and envelope type
// that always accompany it, and what it means. Codes are stable; messages are
// not.
type ErrorCodeInfo struct {
	Code    ErrorCode
	Status  int
	Type    ErrorType
	Meaning string
}

// Generic codes: what a refusal answers when nothing more specific applies.
const (
	CodeInvalidParam           ErrorCode = "invalid_param"
	CodeAuthenticationRequired ErrorCode = "authentication_required"
	CodeResourceAccessDenied   ErrorCode = "resource_access_denied"
	CodeResourceNotFound       ErrorCode = "resource_not_found"
	CodeResourceConflict       ErrorCode = "resource_conflict"
	CodePaymentFailed          ErrorCode = "payment_failed"
	CodeRateLimitExceeded      ErrorCode = "rate_limit_exceeded"
	CodeInternalError          ErrorCode = "internal_error"
	CodeServiceUnavailable     ErrorCode = "service_unavailable"
	// CodeProviderWritesDisabled: the merchant's provider writes are off
	// (provider_write_mode, or the merchant's write posture); the message
	// names why.
	CodeProviderWritesDisabled ErrorCode = "provider_writes_disabled"
	CodeIdempotencyKeyReused   ErrorCode = "idempotency_key_reused"
	CodeIdempotencyKeyInUse    ErrorCode = "idempotency_key_in_use"
	CodeInsufficientCredits    ErrorCode = "insufficient_credits"
	CodeInsufficientFunds      ErrorCode = "insufficient_funds"
	// CodeUsageNotPriced: a priced meter cannot price a usage event.
	CodeUsageNotPriced ErrorCode = "usage_not_priced"
	// CodeEventStoreRequired: the feature needs the event store, which the
	// deployment does not have (Deps.ClickHouse).
	CodeEventStoreRequired ErrorCode = "event_store_required"
	// Orders.
	CodeAlreadyOwned             ErrorCode = "already_owned"
	CodeEntitlementRequired      ErrorCode = "entitlement_required"
	CodeOrderLineUnavailable     ErrorCode = "order_line_unavailable"
	CodeQuantityNotAllowed       ErrorCode = "quantity_not_allowed"
	CodeOrderTotalChanged        ErrorCode = "order_total_changed"
	CodeOrderPaymentInProgress   ErrorCode = "order_payment_in_progress"
	CodeOrderNotPayable          ErrorCode = "order_not_payable"
	CodeOrderNotCancelable       ErrorCode = "order_not_cancelable"
	CodeOrderActionNotAllowed    ErrorCode = "order_action_not_allowed"
	CodePaymentOptionUnavailable ErrorCode = "payment_option_unavailable"
	// Payments recorded outside OpenRails.
	CodePaymentExceedsDue     ErrorCode = "payment_exceeds_due"
	CodeOrderHasRecurringLine ErrorCode = "order_has_recurring_line"
	// CodeModelUnavailable: the language model behind an ask or generate
	// route did not answer.
	CodeModelUnavailable ErrorCode = "model_unavailable"
)

// Refusals of buying a product again while a canceled subscription to it is
// still paid; nothing is charged.
const (
	// CodeSubscriptionResumable: resume the canceled subscription instead.
	CodeSubscriptionResumable ErrorCode = "subscription_resumable"
	// CodeSubscriptionPaidThrough: buy again once its paid period ends.
	CodeSubscriptionPaidThrough ErrorCode = "subscription_paid_through"
)

// Scheduled changes and price migrations.
const (
	CodeScheduledChangeExists       ErrorCode = "scheduled_change_exists"
	CodePriceMigrationNotFound      ErrorCode = "price_migration_not_found"
	CodePriceChangeCurrencyMismatch ErrorCode = "price_change_currency_mismatch"
	CodePriceChangeTargetArchived   ErrorCode = "price_change_target_archived"
	CodePriceIncreaseNoticeTooShort ErrorCode = "price_increase_notice_too_short"
)

// CodeCatalogBenefitOverlap: two recurring products would grant one
// entitlement outside a shared tier group.
const CodeCatalogBenefitOverlap ErrorCode = "catalog_benefit_overlap"

// CodeRevisionMismatch: an edit's expected_revision is not the object's
// current revision; metadata.revision is the current one.
const CodeRevisionMismatch ErrorCode = "revision_mismatch"

// CodeLimitNotFound: the catalog declares no limit under the key.
const CodeLimitNotFound ErrorCode = "limit_not_found"

// Lease codes.
const (
	// CodeLeaseNotFound: the lease id is a UUIDv7 older than the replay
	// horizon and no lease of the merchant has it: it was forgotten, and its
	// items cannot be settled again safely.
	CodeLeaseNotFound ErrorCode = "lease_not_found"
	// CodeLeaseNotHeld: an admission names a lease that is closed or that
	// never synced, so it cannot own the hold.
	CodeLeaseNotHeld ErrorCode = "lease_not_held"
	// CodeUsageOutOfOrder: a usage item skips a number the lease has not
	// settled.
	CodeUsageOutOfOrder ErrorCode = "usage_out_of_order"
)

// CodePSPProbeLimited: the provider refused this login twice within the hour,
// or it is being checked, and the provider locks out repeated failed logins;
// it is not asked again before Retry-After.
const CodePSPProbeLimited ErrorCode = "psp_probe_limited"

// CodeProductInUse: a record refers to the product or one of its prices (a
// purchase, subscription, grant, access, pay URL, migration, provider write,
// PSP link or rate card), so it is never deleted; archive it instead.
const CodeProductInUse ErrorCode = "product_in_use"

// Request-shape codes: the request never reached its operation.
const (
	// CodeUnknownField: the JSON body names a field the route does not
	// accept; param is the field.
	CodeUnknownField ErrorCode = "unknown_field"
	// CodeInvalidQuery: a query parameter is malformed or out of range;
	// param is the parameter.
	CodeInvalidQuery ErrorCode = "invalid_query"
	// CodeInvalidCursor: the list cursor is not one this list issued.
	CodeInvalidCursor        ErrorCode = "invalid_cursor"
	CodeUnsupportedMediaType ErrorCode = "unsupported_media_type"
	CodeRouteNotFound        ErrorCode = "route_not_found"
	CodeMethodNotAllowed     ErrorCode = "method_not_allowed"
	// CodeMerchantMismatch: the credential's merchant differs from the
	// request's expected merchant; no operation was executed.
	CodeMerchantMismatch ErrorCode = "merchant_mismatch"
)

// Authentication and authorization codes: why a credential was refused.
const (
	CodeCredentialExpired                    ErrorCode = "credential_expired"
	CodeCredentialRevoked                    ErrorCode = "credential_revoked"
	CodeServiceCredentialCustomerScopeDenied ErrorCode = "service_credential_customer_scope_denied"
	CodeHostPrincipalInvalid                 ErrorCode = "host_principal_invalid"
	CodePermissionRequired                   ErrorCode = "permission_required"
	CodeMerchantUnresolved                   ErrorCode = "merchant_unresolved"
	CodeCustomerIssuerMismatch               ErrorCode = "customer_issuer_mismatch"
	CodeCustomerTenantMismatch               ErrorCode = "customer_tenant_mismatch"
	CodeInvokerScopedPrincipal               ErrorCode = "invoker_scoped_principal"
	CodeApplicationRequired                  ErrorCode = "application_required"
	CodeStepUpRequired                       ErrorCode = "step_up_required"
	CodeStepUpUnavailable                    ErrorCode = "step_up_unavailable"
	CodeAuthenticationUnavailable            ErrorCode = "authentication_unavailable"
	CodeAuthorizationUnavailable             ErrorCode = "authorization_unavailable"
	CodeCustomerSessionRequired              ErrorCode = "customer_session_required"
)

// Request transport codes.
const (
	CodeDatabaseBusy           ErrorCode = "database_busy"
	CodeIdempotencyKeyRequired ErrorCode = "idempotency_key_required"
	CodeCSRFOriginDenied       ErrorCode = "csrf_origin_denied"
	CodeCaptchaRequired        ErrorCode = "captcha_required"
	CodeCaptchaInvalid         ErrorCode = "captcha_invalid"
	CodeCardRequiresHTTPS      ErrorCode = "card_requires_https"
)

// Payment and payment-method codes.
const (
	CodePaymentMethodNotPSPVaulted     ErrorCode = "payment_method_not_psp_vaulted"
	CodePaymentMethodNotUsable         ErrorCode = "payment_method_not_usable"
	CodePaymentMethodDeleteUnsupported ErrorCode = "payment_method_delete_unsupported"
	CodePaymentMethodDeleteFailed      ErrorCode = "payment_method_delete_failed"
	CodePaymentMethodUpdateUnsupported ErrorCode = "payment_method_update_unsupported"
	CodePaymentMethodUpdateFailed      ErrorCode = "payment_method_update_failed"
	CodeProviderOutcomeUnknown         ErrorCode = "provider_outcome_unknown"
	CodeCardNotSaved                   ErrorCode = "card_not_saved"
	CodeCardAttemptsBlocked            ErrorCode = "card_attempts_blocked"
	CodeCustodianCaptureUnavailable    ErrorCode = "custodian_capture_unavailable"
	CodeCustomerPaymentUnsupported     ErrorCode = "customer_payment_unsupported"
	CodeInvalidPaymentMethod           ErrorCode = "invalid_payment_method"
	CodePaymentIdempotencyConflict     ErrorCode = "payment_idempotency_conflict"
	CodePaymentInProgress              ErrorCode = "payment_in_progress"
	CodePaymentNotRetryable            ErrorCode = "payment_not_retryable"
	CodePaymentNotFound                ErrorCode = "payment_not_found"
	CodeRefundRailUnavailable          ErrorCode = "refund_rail_unavailable"
	CodeRefundUnsupported              ErrorCode = "refund_unsupported"
	CodePaymentNotRefundable           ErrorCode = "payment_not_refundable"
	CodeRefundFailed                   ErrorCode = "refund_failed"
)

// Pay URL and order attempt codes.
const (
	CodePayURLNotFound          ErrorCode = "pay_url_not_found"
	CodePayURLExpired           ErrorCode = "pay_url_expired"
	CodePayURLUnavailable       ErrorCode = "pay_url_unavailable"
	CodePayURLAttemptsExhausted ErrorCode = "pay_url_attempts_exhausted"
	CodeEmbedOriginsRequired    ErrorCode = "embed_origins_required"
	CodeOrderAttemptClosed      ErrorCode = "order_attempt_closed"
	CodeOrderAttemptExpired     ErrorCode = "order_attempt_expired"
)

// Subscription codes.
const (
	CodeSubscriptionNotFound     ErrorCode = "subscription_not_found"
	CodeSubscriptionNotActive    ErrorCode = "subscription_not_active"
	CodeCancelUnsupportedOnRail  ErrorCode = "cancel_unsupported_on_rail"
	CodeRebillTermsCommitted     ErrorCode = "rebill_terms_committed"
	CodeCustomerEmailRequired    ErrorCode = "customer_email_required"
	CodeSolanaTransactionRefused ErrorCode = "solana_transaction_refused"
	CodeSolanaRPCUnavailable     ErrorCode = "solana_rpc_unavailable"
)

// Credit, admission and customer codes.
const (
	CodeCreditGrantNotFound            ErrorCode = "credit_grant_not_found"
	CodeCreditGrantHeld                ErrorCode = "credit_grant_held"
	CodeCreditGrantUnavailable         ErrorCode = "credit_grant_unavailable"
	CodeAdmissionNotFound              ErrorCode = "admission_not_found"
	CodeAdmissionCaptured              ErrorCode = "admission_captured"
	CodeHoldNotFound                   ErrorCode = "hold_not_found"
	CodeCurrencyUnsupported            ErrorCode = "currency_unsupported"
	CodeMerchantUnitUnsupported        ErrorCode = "merchant_unit_unsupported"
	CodeRunningChargeNotFound          ErrorCode = "running_charge_not_found"
	CodeInvalidSettlementStatusRequest ErrorCode = "invalid_settlement_status_request"
	CodeCustomerNotFound               ErrorCode = "customer_not_found"
	CodeInvalidCustomerID              ErrorCode = "invalid_customer_id"
	CodeBillingPolicyNotFound          ErrorCode = "billing_policy_not_found"
	CodeHostEventNotFound              ErrorCode = "host_event_not_found"
)

// Catalog and metering codes.
const (
	CodeProductNotFound          ErrorCode = "product_not_found"
	CodePriceNotFound            ErrorCode = "price_not_found"
	CodePriceKeyNotFound         ErrorCode = "price_key_not_found"
	CodePriceKeyCadenceConflict  ErrorCode = "price_key_cadence_conflict"
	CodePriceNotSellable         ErrorCode = "price_not_sellable"
	CodeProviderPriceUnreadable  ErrorCode = "provider_price_unreadable"
	CodeTrialUnsupportedOnRail   ErrorCode = "trial_unsupported_on_rail"
	CodeProductTierGroupConflict ErrorCode = "product_tier_group_conflict"
	CodeProductTierGroupInUse    ErrorCode = "product_tier_group_in_use"
	CodeCatalogRevisionConflict  ErrorCode = "catalog_revision_conflict"
	CodeUsageMeterNotFound       ErrorCode = "usage_meter_not_found"
	CodeDefaultRateCardNotFound  ErrorCode = "default_rate_card_not_found"
	CodeRateCardProductNotFound  ErrorCode = "rate_card_product_not_found"
	CodeUsageRateCardInvalid     ErrorCode = "usage_rate_card_invalid"
	CodeUsageMeterInvalid        ErrorCode = "usage_meter_invalid"
	CodeMeterInUse               ErrorCode = "meter_in_use"
	CodeMeterRateCardConflict    ErrorCode = "meter_rate_card_conflict"
	CodePurchaseReviewResolved   ErrorCode = "purchase_review_resolved"
)

// Finding, merchant configuration and PSP codes.
const (
	CodeFindingNotActionable     ErrorCode = "finding_not_actionable"
	CodeFindingActionFailed      ErrorCode = "finding_action_failed"
	CodePSPNotFound              ErrorCode = "psp_not_found"
	CodePSPExists                ErrorCode = "psp_exists"
	CodePSPKeyTaken              ErrorCode = "psp_key_taken"
	CodePSPCredentialsRejected   ErrorCode = "psp_credentials_rejected"
	CodePSPEnvironmentRefused    ErrorCode = "psp_environment_refused"
	CodePSPLastActive            ErrorCode = "psp_last_active"
	CodePSPClaimRequiresProof    ErrorCode = "psp_claim_requires_proof"
	CodeInvalidPSPReference      ErrorCode = "invalid_psp_reference"
	CodeMerchantConfigReadOnly   ErrorCode = "merchant_config_read_only"
	CodePlatformVaultUnavailable ErrorCode = "platform_vault_unavailable"
	CodePlatformVaultMixedClass  ErrorCode = "platform_vault_mixed_class"
	CodePlatformVaultUnreachable ErrorCode = "platform_vault_unreachable"
	CodeCardVaultUnknown         ErrorCode = "card_vault_unknown"
	CodeChargePSPUnknown         ErrorCode = "charge_psp_unknown"
	CodeChargePSPIncompatible    ErrorCode = "charge_psp_incompatible"
	CodeWebhookInvalid           ErrorCode = "webhook_invalid"
	CodeWebhookAccountMismatch   ErrorCode = "webhook_account_mismatch"
	CodeMetricsQueryInvalid      ErrorCode = "metrics_query_invalid"
	CodeDashboardInvalid         ErrorCode = "dashboard_invalid"
	CodeMessageTemplateInvalid   ErrorCode = "message_template_invalid"
	CodeAlertEmailRequired       ErrorCode = "alert_email_required"
	CodeMessageSendFailed        ErrorCode = "message_send_failed"
)

// Billing import and archive codes.
const (
	CodeAsOfRequired                   ErrorCode = "as_of_required"
	CodeBillingArchiveUnavailable      ErrorCode = "billing_archive_unavailable"
	CodeBillingArchiveInvalidArtifact  ErrorCode = "billing_archive_invalid_artifact"
	CodeBillingArchiveMerchantMismatch ErrorCode = "billing_archive_merchant_mismatch"
	CodeBillingArchiveNotEmpty         ErrorCode = "billing_archive_not_empty"
	CodeBillingArchiveUnsupportedState ErrorCode = "billing_archive_unsupported_state"
	CodeBillingArchivePlatformCards    ErrorCode = "billing_archive_platform_cards"
	CodeBillingArchiveIntegrity        ErrorCode = "billing_archive_integrity"
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
var errorCodes = []ErrorCodeInfo{
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
	{CodeDatabaseBusy, 503, fault, "No database connection is available; retry shortly."},
	{CodeModelUnavailable, 502, fault, "The language model did not answer; retry, or ask a narrower question."},

	// Request shape and transport.
	{CodeUnknownField, 400, invalid, "The JSON body names a field the route does not accept; param is the field."},
	{CodeInvalidQuery, 400, invalid, "A query parameter is malformed or out of range; param is the parameter."},
	{CodeInvalidCursor, 400, invalid, "The cursor is not one this list issued."},
	{CodeEventCursorExpired, 410, invalid, "The event cursor is no longer retained; resynchronize current state."},
	{CodeInvalidRequestBody, 400, invalid, "The request body could not be read or is not one JSON value."},
	{CodeRequestBodyTooLarge, 413, invalid, "The request body exceeds the deployment's cap."},
	{CodeUnsupportedMediaType, 415, invalid, "The request body is not application/json."},
	{CodeRouteNotFound, 404, invalid, "No route matches the path."},
	{CodeMethodNotAllowed, 405, invalid, "The path exists but not for this method; Allow lists its methods."},
	{CodeMerchantMismatch, 412, invalid, "The credential is bound to a different merchant than OpenRails-Expected-Merchant; the operation was not executed."},
	{CodeIdempotencyKeyRequired, 400, invalid, "The operation needs an Idempotency-Key header."},
	{CodeCSRFOriginDenied, 403, authz, "A cookie-authenticated request came from an origin that is not allowed."},
	{CodeCaptchaRequired, 403, invalid, "The caller must solve a captcha and resend with its token."},
	{CodeCaptchaInvalid, 403, invalid, "The captcha token was rejected."},
	{CodeCardRequiresHTTPS, 400, invalid, "Card data is accepted only over HTTPS."},

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
	{CodeCustomerTenantMismatch, 403, authz, "The signed-in account does not belong to this merchant's tenant."},
	{CodeServiceCredentialCustomerScopeDenied, 403, authz, "The service credential may not act for this customer."},
	{CodeInvokerScopedPrincipal, 403, authz, "An invoker-scoped credential spends a customer's balance but may not manage the account."},
	{CodeApplicationRequired, 403, authz, "The route is your backend's: it takes an application's credential, never a person's."},
	{CodePersonRequired, 403, authz, "Only a person signed in, or the embedding host in process, may take this action; an API key or service token may not."},
	{CodeStepUpUnavailable, 403, authz, "The operation needs a recent sign-in and this credential cannot prove one."},
	{CodeAuthorizationUnavailable, 503, fault, "Permissions could not be checked right now; retry."},
	{CodeCustomerActionRequired, 403, authz, "Only the customer may take this action, through their own step."},
	{CodeCustomerSessionRequired, 403, authz, "The operation needs the customer's interactive session."},

	// Payments and payment methods.
	{CodeCardDeclined, 402, card, "The provider declined the card; metadata.decline_reason says why."},
	{CodePaymentMethodStale, 402, card, "The saved payment method can no longer be charged; collect the card again."},
	{CodePaymentProviderRejected, 502, fault, "The provider refused to process the charge for a gateway or account reason."},
	{CodePaymentMethodRequired, 400, invalid, "The charge names neither a saved payment method nor a new card token."},
	{CodePaymentDuplicateRefused, 409, invalid, "The provider refused an identical charge it had just made; retry after its duplicate window."},
	{CodePaymentMethodSameVault, 409, invalid, "The card is in the provider vault the subscription already bills."},
	{CodePaymentMethodPSPMismatch, 409, invalid, "The payment method is vaulted by another PSP than the subscription's."},
	{CodePaymentMethodNotPSPVaulted, 409, invalid, "The payment method is not held in the provider vault."},
	{CodePaymentMethodNotUsable, 409, invalid, "The payment method cannot be charged in its current state."},
	{CodePaymentMethodDeleteUnsupported, 400, invalid, "This payment method cannot be deleted through OpenRails."},
	{CodePaymentMethodDeleteFailed, 502, fault, "The provider could not delete the payment method."},
	{CodePaymentMethodUpdateUnsupported, 400, invalid, "This payment method cannot be updated through OpenRails."},
	{CodePaymentMethodUpdateFailed, 502, fault, "The provider could not update the payment method."},
	{CodeProviderOutcomeUnknown, 409, fault, "The provider did not confirm the outcome; read the resource before retrying."},
	{CodeCardNotSaved, 409, fault, "The card was not saved; enter it again."},
	{CodeCardAttemptsBlocked, 429, limit, "Too many declined card attempts; try again later."},
	{CodeCustodianCaptureUnavailable, 503, fault, "The card custodian cannot capture cards right now."},
	{CodeCustomerPaymentUnsupported, 400, invalid, "Customer-present payment is unsupported for this rail or method."},
	{CodeInvalidPaymentMethod, 400, invalid, "The payment method is not eligible for this operation."},
	{CodePaymentIdempotencyConflict, 409, invalid, "The idempotency key belongs to another payment request."},
	{CodePaymentInProgress, 409, invalid, "A payment is already unresolved; read the resource before retrying."},
	{CodePaymentNotRetryable, 409, invalid, "The resource is not payable now."},
	{CodePaymentNotFound, 404, invalid, "The payment does not exist."},
	{CodeRefundRailUnavailable, 409, invalid, "The payment's rail cannot accept a refund right now."},
	{CodeRefundUnsupported, 400, invalid, "The payment's rail has no automatic refund."},
	{CodePaymentNotRefundable, 400, invalid, "The payment is not a completed rail charge, or the amount exceeds what remains refundable."},
	{CodeRefundFailed, 502, fault, "The provider refused the refund."},

	// Pay URLs.
	{CodePayURLNotFound, 404, invalid, "The pay URL is unknown or no longer valid."},
	{CodePayURLExpired, 410, invalid, "The pay URL expired; the merchant creates a new order."},
	{CodePayURLUnavailable, 400, invalid, "This deployment serves no checkout origin, or the order cannot take a pay URL."},
	{CodePayURLAttemptsExhausted, 409, invalid, "The pay URL took its most card attempts; the merchant creates a new order."},
	{CodeEmbedOriginsRequired, 400, invalid, "An embedded pay URL needs the origins that may frame it: the merchant's settings checkout.embed_origins."},
	{CodeOrderAttemptClosed, 409, invalid, "The order attempt already completed or was canceled."},
	{CodeOrderAttemptExpired, 410, invalid, "The order attempt expired before it was paid."},

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
	{CodeSubscriptionNotFound, 404, invalid, "The subscription does not exist."},
	{CodeSubscriptionNotActive, 409, invalid, "The subscription is not active."},
	{CodeCancelUnsupportedOnRail, 400, invalid, "This rail has no cancel operation."},
	{CodeProviderCancelHeld, 409, invalid, "Cancelling needs a destructive provider action that is not armed for this merchant."},
	{CodeSubscriptionResumable, 409, invalid, "A canceled subscription to this product is still paid and can be resumed; resume it instead of buying again."},
	{CodeSubscriptionPaidThrough, 409, invalid, "A canceled subscription to this product is still paid; buy again once its paid period ends."},
	{CodeRebillTermsCommitted, 409, invalid, "An accepted recurring payment owns the pending price terms."},
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
	{CodeCustomerEmailRequired, 400, invalid, "The rail needs the customer's verified email and username."},
	{CodeSolanaTransactionRefused, 400, invalid, "The wallet transaction could not be prepared or confirmed; the message says why."},
	{CodeSolanaRPCUnavailable, 502, fault, "The Solana RPC endpoints did not answer; retry."},
	{CodeScheduledChangeExists, 409, invalid, "The subscription already has a scheduled change."},
	{CodePriceMigrationNotFound, 404, invalid, "The price migration does not exist."},
	{CodePriceChangeCurrencyMismatch, 422, invalid, "The target price must be in the subscription's currency."},
	{CodePriceChangeTargetArchived, 422, invalid, "The target price is archived."},
	{CodePriceIncreaseNoticeTooShort, 422, invalid, "effective_at is inside the merchant's notice window for a price increase."},

	// Collection.
	{CodeDefaultPaymentMethodRequired, 400, invalid, "The customer has no default card for the currency."},
	{CodeDefaultPaymentMethodInvalid, 400, invalid, "The card cannot be the customer's default for the currency."},

	// Credits and admission.
	{CodeCreditGrantNotFound, 404, invalid, "The credit grant does not exist."},
	{CodeCreditGrantHeld, 409, invalid, "Active holds need the grant's remaining credit."},
	{CodeCreditGrantUnavailable, 409, invalid, "The credit grant expired, ended or has no remaining credit."},
	{CodeAdmissionNotFound, 404, invalid, "No admission was made under this request id."},
	{CodeAdmissionCaptured, 409, invalid, "The admission was captured; it can no longer be released."},
	{CodeHoldNotFound, 404, invalid, "The admission holds nothing open: it was captured, released or lapsed."},
	{CodeCurrencyUnsupported, 400, invalid, "The currency is not in OpenRails' registry."},
	{CodeMerchantUnitUnsupported, 400, invalid, "The currency is one of the merchant's own units, which the operation does not take: it takes a currency from OpenRails' registry."},
	{CodeRunningChargeNotFound, 404, invalid, "No running charge was started under this reference."},
	{CodeInvalidSettlementStatusRequest, 400, invalid, "A settlement status read needs a customer and a price."},

	// Customers.
	{CodeCustomerNotFound, 404, invalid, "The customer does not exist."},
	{CodeInvalidCustomerID, 400, invalid, "The customer id is missing or malformed."},
	{CodeBillingPolicyNotFound, 404, invalid, "The billing policy does not exist."},
	{CodeHostEventNotFound, 404, invalid, "The host event does not exist."},

	// Catalog and metering.
	{CodeProductNotFound, 404, invalid, "The product does not exist."},
	{CodeProductInUse, 409, invalid, "Records refer to the product; archive it instead."},
	{CodePriceNotFound, 404, invalid, "The price does not exist."},
	{CodePriceKeyNotFound, 404, invalid, "No price holds this key."},
	{CodePriceKeyCadenceConflict, 409, invalid, "The product's default price key is held by a price on another cadence."},
	{CodePriceNotSellable, 400, invalid, "None of the price's methods can sell it."},
	{CodeProviderPriceUnreadable, 502, fault, "The provider did not answer for the provider price's terms; retry."},
	{CodeTrialUnsupportedOnRail, 400, invalid, "This rail cannot run a trial first phase."},
	{CodeProductTierGroupConflict, 409, invalid, "A customer holds live subscriptions to more than one product of the tier group."},
	{CodeProductTierGroupInUse, 409, invalid, "The tier group cannot change while a subscription has a plan change in flight."},
	{CodeRevisionMismatch, 409, invalid, "The object changed since the revision the edit expected; metadata.revision is the current one."},
	{CodeCatalogRevisionConflict, 409, invalid, "The catalog changed during the application; retry."},
	{CodeUsageMeterNotFound, 404, invalid, "The usage meter does not exist."},
	{CodeDefaultRateCardNotFound, 404, invalid, "The meter has no default rate card."},
	{CodeRateCardProductNotFound, 404, invalid, "The rate card names a product that does not exist."},
	{CodeUsageRateCardInvalid, 400, invalid, "The usage rate card is invalid."},
	{CodeUsageMeterInvalid, 400, invalid, "The meter definition is invalid."},
	{CodeEventStoreRequired, 409, invalid, "The deployment has no event store (ClickHouse): usage reporting, admissions, meters, limits, usage rate cards, spend windows, failed-usage windows, the accrual-rate cap and usage history are off."},
	{CodeUsageNotPriced, 422, invalid, "A priced meter cannot price the usage (no matrix cell for its value, or no rate card in its currency); param names the field."},
	{CodeMeterInUse, 409, invalid, "The meter is referenced and cannot change this way."},
	{CodeMeterRateCardConflict, 409, invalid, "The meter and its rate card disagree."},
	{CodeLimitNotFound, 404, invalid, "The catalog declares no limit under this key."},
	{CodeLeaseNotFound, 404, invalid, "No lease of the merchant has this id, which is older than the replay horizon; start a new lease."},
	{CodeLeaseNotHeld, 409, invalid, "The admission names a lease that is closed or never synced, so it cannot own the hold."},
	{CodeUsageOutOfOrder, 409, invalid, "A usage item skips a number the lease has not settled; send the items in order."},
	{CodePurchaseReviewResolved, 409, invalid, "The purchase review was already resolved."},

	// Findings.
	{CodeFindingNotActionable, 422, invalid, "The finding carries no recommendation to approve; ignore it or fix it out of band."},
	{CodeFindingActionFailed, 502, fault, "Running the finding's recommendation failed; the finding stays open with the error in its notes."},
	{CodeFindingRequiresMerchantConfig, 409, invalid, "The finding is a pull.* proposal: it is approved or dismissed as a proposal, with merchant config."},
	{CodeDestructiveActionsHeld, 409, invalid, "The kill switch holds changes that end access or write to a provider; the proposal stays open."},
	{CodeProviderWritesHeld, 409, invalid, "The merchant's readonly write posture holds changes that move money or write to a provider; the proposal stays open."},

	// Merchant configuration and PSPs.
	{CodePSPNotFound, 404, invalid, "The PSP does not exist."},
	{CodePSPImportInProgress, 409, invalid, "Another import of the PSP is pulling or applying; wait for it to finish."},
	{CodePSPExists, 409, invalid, "The account is already a PSP, of this merchant or another; update it instead."},
	{CodePSPKeyTaken, 409, invalid, "Another live PSP holds the key."},
	{CodePSPCredentialsRejected, 400, invalid, "The provider rejected the credentials, or answered that they are not in the PSP's declared environment."},
	{CodePSPEnvironmentRefused, 409, invalid, "A live PSP is refused where the deployment wires fake providers."},
	{CodePSPLastActive, 409, invalid, "The PSP is the last active one on its rail; pass allow_last to archive it."},
	{CodePSPClaimRequiresProof, 403, authz, "Claiming a provider account needs credentials that prove control of it."},
	{CodePSPProbeLimited, 429, limit, "The provider refused this login twice within the hour, or it is being checked, and it locks out repeated failed logins; Retry-After says when it may be checked again."},
	{CodeInvalidPSPReference, 400, invalid, "The PSP reference is invalid."},
	{CodeMerchantConfigReadOnly, 409, invalid, "The merchant's configuration is read from a file; change the file."},
	{CodePlatformVaultUnavailable, 409, invalid, "The deployment has no platform vault for its posture, or its identity for this merchant names another vault tenant; save cards at a PSP's vault."},
	{CodePlatformVaultMixedClass, 409, invalid, "The platform vault's charge PSPs mix live and test; one platform identity holds one money class."},
	{CodePlatformVaultUnreachable, 409, invalid, "A charge PSP cannot charge cards from the platform vault (Stripe, or card_entry server); leave it out of charge_psps or save cards at a PSP's vault."},
	{CodeCardVaultUnknown, 409, invalid, "settings.card_vault names no live card PSP of the merchant, nor platform."},
	{CodeChargePSPUnknown, 409, invalid, "settings.charge_psps names a PSP that is not a live card PSP of the merchant."},
	{CodeChargePSPIncompatible, 409, invalid, "A charge PSP cannot charge cards from the card vault: a PSP-vaulted card charges only through its own account, a custodian's card through the PSPs naming that custodian."},
	{CodeWebhookInvalid, 400, invalid, "The outbound webhook is invalid."},
	{CodeWebhookAccountMismatch, 400, invalid, "The webhook's account does not match its payload."},
	{CodeMetricsQueryInvalid, 400, invalid, "The metrics query is invalid; metadata.errors lists why."},
	{CodeDashboardInvalid, 400, invalid, "The dashboard is invalid; metadata.errors lists why."},
	{CodeMessageTemplateInvalid, 400, invalid, "The message template does not render against its event type's sample data; the message says why."},
	{CodeAlertEmailRequired, 409, invalid, "The merchant has no alert_email to send a test message to."},
	{CodeMessageSendFailed, 502, fault, "The email sender did not accept the message."},

	// Billing import and archive.
	{CodeAsOfRequired, 400, invalid, "The import needs as_of, its RFC 3339 evidence horizon."},
	{CodeBillingArchiveUnavailable, 500, fault, "The billing archive operation failed."},
	{CodeBillingArchiveInvalidArtifact, 400, invalid, "The billing archive is invalid, incomplete or unsupported."},
	{CodeBillingArchiveMerchantMismatch, 409, invalid, "The archive's merchant is not the destination's."},
	{CodeBillingArchiveNotEmpty, 409, invalid, "The destination's billing state must be empty."},
	{CodeBillingArchiveUnsupportedState, 409, invalid, "The merchant has state that cannot safely be moved."},
	{CodeBillingArchivePlatformCards, 409, invalid, "The merchant holds cards in the platform vault; export or remove them first."},
	{CodeBillingArchiveIntegrity, 422, invalid, "The billing archive failed integrity validation."},

	// Customer links (openrails.dev).
}

var errorCodeIndex = func() map[ErrorCode]ErrorCodeInfo {
	index := make(map[ErrorCode]ErrorCodeInfo, len(errorCodes))
	for _, c := range errorCodes {
		if _, dup := index[c.Code]; dup {
			panic("billing: error code registered twice: " + string(c.Code))
		}
		if c.Status < 400 || c.Status > 599 || http.StatusText(c.Status) == "" || c.Type == "" || c.Meaning == "" {
			panic("billing: incomplete error code: " + string(c.Code))
		}
		index[c.Code] = c
	}
	return index
}()

// ErrorCodes lists every registered error code, sorted by code.
func ErrorCodes() []ErrorCodeInfo {
	out := append([]ErrorCodeInfo(nil), errorCodes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// LookupErrorCode returns a code's registration.
func LookupErrorCode(code ErrorCode) (ErrorCodeInfo, bool) {
	c, ok := errorCodeIndex[code]
	return c, ok
}
