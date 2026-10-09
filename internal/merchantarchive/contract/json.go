package contract

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db/models"
)

var initialMembershipTermsJSON = object(map[string]jsonRule{"collection_policy": textValue, "subscription_id": uuidValue, "payment_id": uuidValue, "customer_id": uuidValue, "psp_id": uuidValue, "product_id": uuidValue, "price_id": uuidValue, "payment_method_id": uuidValue, "product_name": textValue, "quantity": integerValue, "amount": moneyStringValue, "recurring_amount": moneyStringValue, "currency": textValue, "accepted_at": textValue, "period_start": textValue, "period_end": textValue, "pending": booleanValue, "cancel_after_initial": booleanValue, "access_duration_hours": nullable(integerValue), "entitlements": acceptedEntitlementsJSON, "legacy_entitlements": dictionary(nullable(integerValue)),
	"replaces": object(map[string]jsonRule{"subscription_id": uuidValue, "price_id": uuidValue, "period_end": textValue, "credit": moneyStringValue}),
	"adds":     object(map[string]jsonRule{"from_quantity": integerValue})})

var creditGrantJSON = nullable(func(v any) bool {
	if !object(map[string]jsonRule{"amount": moneyStringValue, "currency": textValue, "expires_after_days": integerValue, "starts_at": textValue, "expires_at": nullable(textValue)})(v) {
		return false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var snapshot models.CreditGrantSnapshot
	return json.Unmarshal(raw, &snapshot) == nil && snapshot.Validate() == nil
})

var acceptedPurchaseJSON = object(map[string]jsonRule{
	"price_id": uuidValue, "product_id": uuidValue, "payment_id": uuidValue, "product_key": textValue, "product_name": textValue,
	"amount": moneyStringValue, "currency": textValue, "access_duration_hours": nullable(integerValue), "entitlements": nullable(acceptedEntitlementsJSON), "legacy_entitlements": dictionary(nullable(integerValue)),
	"accepted_at": textValue, "entitlement_start": textValue, "credit_grant": creditGrantJSON,
	"psp_links": dictionary(object(map[string]jsonRule{"psp_id": uuidValue, "rail": textValue, "plan_id": textValue, "form_name": textValue, "flex_id": textValue, "price_id": textValue, "product_id": textValue, "provider": textValue, "recurring_billing_option_id": textValue})),
})

var acceptedRenewalJSON = object(map[string]jsonRule{
	"psp_id": uuidValue, "subscription_id": uuidValue, "customer_id": uuidValue,
	"from_price_id": uuidValue, "from_product_id": uuidValue, "price_id": uuidValue, "product_id": uuidValue,
	"product_name": textValue, "quantity": integerValue, "amount": moneyStringValue, "currency": textValue,
	"period_start": textValue, "period_end": textValue, "access_duration_hours": nullable(integerValue),
	"entitlements": nullable(acceptedEntitlementsJSON), "legacy_entitlements": dictionary(nullable(integerValue)), "previous_entitlements": nullable(acceptedEntitlementsJSON),
	"scheduled_change_id": uuidValue,
})

// mandateJSON is the mandate lineage an operation cites; it is not card data.
var mandateJSON = object(map[string]jsonRule{"id": uuidValue, "kind": textValue, "initial_transaction_id": textValue, "network_transaction_id": textValue, "transaction_link_id": textValue})
var frozenInstrumentJSON = object(map[string]jsonRule{"psp_id": uuidValue, "custodian": textValue, "custodian_id": uuidValue, "rail_customer_ref": textValue, "rail_method_ref": textValue, "mandate": mandateJSON})

type jsonRule func(any) bool

// Accepted payloads retain their old map representation as historical evidence.
// New catalog and stored snapshot contracts accept only lists.
func acceptedEntitlementsJSON(v any) bool {
	return array(textValue)(v) || dictionary(nullable(integerValue))(v)
}

// Application metadata is opaque JSON. The bounded parser validates its syntax;
// the archive preserves it without interpreting keys or filtering string values.
func metadataJSON(any) bool { return true }

func textValue(v any) bool { s, ok := v.(string); return ok && safeText(s) }
func uuidValue(v any) bool { s, ok := v.(string); return ok && uuidPattern.MatchString(s) }
func sha256Value(v any) bool {
	s, ok := v.(string)
	if !ok || len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func integerValue(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	_, err := strconv.ParseInt(n.String(), 10, 64)
	return err == nil
}
func moneyStringValue(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && strconv.FormatInt(n, 10) == s
}
func booleanValue(v any) bool { _, ok := v.(bool); return ok }
func booleanSetting(v any) bool {
	if booleanValue(v) {
		return true
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	_, err := strconv.ParseBool(s)
	return err == nil
}
func integerSetting(v any) bool {
	if integerValue(v) {
		return true
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}
func nullable(r jsonRule) jsonRule { return func(v any) bool { return v == nil || r(v) } }
func object(fields map[string]jsonRule) jsonRule {
	return func(v any) bool {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		for k, x := range m {
			r, ok := fields[k]
			if !ok || !r(x) {
				return false
			}
		}
		return true
	}
}
func dictionary(r jsonRule) jsonRule {
	return func(v any) bool {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		for k, x := range m {
			if !safeText(k) || !r(x) {
				return false
			}
		}
		return true
	}
}
func array(r jsonRule) jsonRule {
	return func(v any) bool {
		a, ok := v.([]any)
		if !ok {
			return false
		}
		for _, x := range a {
			if !r(x) {
				return false
			}
		}
		return true
	}
}

var emptyObject = object(map[string]jsonRule{})

// Provider adapters retain these public routing fields after price binding
// identities move into dedicated columns. Preserve their string values; an
// unrecognized field must be reviewed instead of silently lost in an archive.
var priceBindingConfigurationJSON = object(map[string]jsonRule{
	"product_id": textValue, "provider": textValue, "lookup_key": textValue,
	"form_name": textValue, "enabled": textValue,
	"token": textValue, "mint": textValue, "mint_symbol": textValue,
	"amount_base_units": textValue, "period_hours": textValue,
	"created_at": textValue, "merchant_address": textValue,
})

var budgetWindow = object(map[string]jsonRule{"key": textValue, "window_seconds": integerValue, "limit": integerValue, "currency": textValue})
var profileJSON = object(map[string]jsonRule{"display_name": textValue, "logo_url": textValue, "from_email": textValue, "support_url": textValue, "signup_url": textValue})
var contactsJSON = array(object(map[string]jsonRule{"name": textValue, "email": textValue}))
var operatorResolutionJSON = object(map[string]jsonRule{"actor": textValue, "reason": textValue, "resolved_at": textValue, "step": textValue, "not_executed": booleanValue, "provider_reference": textValue})
var invoiceLineJSON = array(object(map[string]jsonRule{"event_type": textValue, "amount": integerValue, "count": integerValue, "dimensions": dictionary(integerValue)}))
var pspSettingsJSON = object(map[string]jsonRule{"publishable_key": textValue,
	"tokenization_key": textValue, "tokenization_url": textValue, "card_entry": textValue, "rpc_provider": textValue, "recipient_wallet": textValue, "tokens": dictionary(object(map[string]jsonRule{"mint": textValue, "name": textValue}))})
var rateJSON = object(map[string]jsonRule{
	"model": textValue, "currency": textValue,
	"flat": object(map[string]jsonRule{"amount": moneyStringValue}),
	"per_unit": object(map[string]jsonRule{
		"unit_amount": moneyStringValue, "divide_by": integerValue, "round": textValue, "maximum_amount": moneyStringValue,
		"matrix": object(map[string]jsonRule{"dimension": textValue, "cells": dictionary(object(map[string]jsonRule{"unit_amount": moneyStringValue, "maximum_amount": moneyStringValue, "included": integerValue}))}),
	}),
	"tiered":  object(map[string]jsonRule{"mode": textValue, "tiers": array(object(map[string]jsonRule{"up_to": nullable(integerValue), "unit_amount": moneyStringValue, "flat_amount": moneyStringValue}))}),
	"package": object(map[string]jsonRule{"amount": moneyStringValue, "package_size": integerValue, "free_units": integerValue}),
})

var collectedReceiptJSON = object(map[string]jsonRule{
	"stripe_engine": object(map[string]jsonRule{"one_time": booleanValue, "customer_initiated": booleanValue, "refunded_amount_minor": moneyStringValue, "refunded": booleanValue, "disputed": booleanValue, "payment_intent_id": textValue, "charge_id": textValue, "customer_ref": textValue, "method_ref": textValue, "amount_minor": moneyStringValue, "currency": textValue, "merchant_id": uuidValue, "psp_id": uuidValue, "customer_id": uuidValue, "operation_id": uuidValue, "initial": booleanValue, "renewal_terms_sha256": sha256Value}),
	"version":       integerValue, "family": textValue,
	"binding": object(map[string]jsonRule{"operation_id": uuidValue, "merchant_id": uuidValue, "psp_id": uuidValue, "kind": textValue, "payload_sha256": sha256Value}),
	"nmi":     object(map[string]jsonRule{"transaction_id": textValue, "order_reference": textValue, "customer_vault_id": textValue, "vault_billing_id": textValue, "amount": moneyStringValue, "currency": textValue, "approved": booleanValue}),
	"stripe":  object(map[string]jsonRule{"invoice_id": textValue, "status": textValue, "customer_id": textValue, "payment_method_id": textValue, "amount_paid": moneyStringValue, "currency": textValue, "charge_id": textValue, "payment_intent_id": textValue, "collection_key": textValue, "charged_amount": moneyStringValue, "charge_currency": textValue, "charge_customer_id": textValue, "charge_paid": booleanValue, "charge_captured": booleanValue, "charge_status": textValue, "charge_invoice_id": textValue, "charge_payment_intent_id": textValue}),
})

var receiptBindingJSON = object(map[string]jsonRule{"operation_id": uuidValue, "merchant_id": uuidValue, "psp_id": uuidValue, "kind": textValue, "payload_sha256": sha256Value})

// Provider-operation envelopes retain their exact structured shapes.
// An unsupported shape is a refusal, never a lossy rewrite of a replay body.
var instrumentJSON = object(map[string]jsonRule{
	"psp_id": uuidValue, "custodian": textValue, "custodian_id": uuidValue,
	"rail_customer_ref": textValue, "rail_method_ref": textValue,
	"mandate": mandateJSON,
})

// Temporary encrypted capture secrets are deliberately absent. Only terminal
// nonsecret binding/history has a portable shape; semantic validation below
// uses the same decoder as the checkout service.
var captureJSON = object(map[string]jsonRule{
	"merchant_id": uuidValue, "customer_id": uuidValue, "psp_id": uuidValue, "custodian_id": uuidValue,
	"account_id": textValue, "environment": textValue, "profile_id": textValue, "public_api_key": textValue, "api_base_url": textValue, "sdk_url": textValue,
	"vendor_customer_id": textValue, "vendor_session_id": textValue, "expires_at": textValue, "payment_method_id": uuidValue, "vendor_method_id": textValue, "accepted_token_hash": sha256Value,
})

var jsonRules = map[string]jsonRule{
	"products.credit_grant":                             nullable(object(map[string]jsonRule{"currency": textValue, "amount": nullable(moneyStringValue), "from_payment": booleanValue, "expires_after_days": integerValue})),
	"prices.customer_amount":                            nullable(object(map[string]jsonRule{"min_amount": moneyStringValue, "max_amount": moneyStringValue})),
	"prices.quantity":                                   nullable(object(map[string]jsonRule{"min": integerValue, "max": integerValue})),
	"payments.credit_grant_snapshot":                    creditGrantJSON,
	"provider_intents.nmi_vault_delete.payload":         object(map[string]jsonRule{"billing_entry_only": booleanValue, "user_id": uuidValue, "payment_method_id": uuidValue, "rail_customer_ref": textValue, "rail_method_ref": textValue}),
	"provider_intents.nmi_vault_delete.result_evidence": object(map[string]jsonRule{"deleted": booleanValue, "verified_absent": booleanValue, "verified_entry_absent": booleanValue, "already_absent": booleanValue, "no_rail_customer_ref": booleanValue, "vault_id": textValue, "billing_id": textValue, "scoped_to_billing_entry": textValue}),
	"provider_intents.hyperswitch_method_delete.payload": object(map[string]jsonRule{
		"customer_id": uuidValue, "payment_method_id": uuidValue, "instrument": instrumentJSON, "environment": textValue, "detach_only": booleanValue,
		"binding": object(map[string]jsonRule{"account_id": textValue, "profile_id": textValue, "api_base_url": textValue}),
	}),
	"provider_intents.hyperswitch_method_delete.result_evidence": object(map[string]jsonRule{"physically_deleted": booleanValue, "detached": booleanValue, "vendor_method_id": textValue}),

	// The completed collection operation retains its frozen instrument and
	// accepted terms for replay; none of these fields contains card data.
	"provider_intents.invoice_collection.payload": object(map[string]jsonRule{
		"initiator":  textValue,
		"invoice_id": uuidValue, "customer_id": uuidValue, "payment_id": uuidValue, "payment_method_id": uuidValue,
		"rail": textValue, "currency": textValue, "amount": integerValue, "amount_minor": integerValue, "description": textValue, "provider_customer_ref": textValue,
		"hyperswitch": object(map[string]jsonRule{"account_id": textValue, "profile_id": textValue, "api_base_url": textValue}),
		"instrument":  object(map[string]jsonRule{"psp_id": uuidValue, "custodian": textValue, "custodian_id": uuidValue, "rail_customer_ref": textValue, "rail_method_ref": textValue, "mandate": mandateJSON}),
	}),
	"provider_intents.invoice_collection.result_evidence": nullable(object(map[string]jsonRule{
		"qualified_receipt": collectedReceiptJSON,

		"transaction_id": textValue, "external_invoice_id": textValue, "rail": textValue,
		"declined": booleanValue, "failure_code": textValue, "failure_message": textValue, "not_executed": booleanValue,
		"not_executed_code": textValue, "submitted_at": textValue, "provider_contradiction": textValue, "verified_existing": booleanValue,
		"operator_resolution": operatorResolutionJSON,
	})),
	"provider_intents.subscription_collection.payload": object(map[string]jsonRule{
		"initiator": textValue, "requested_payment_method_id": uuidValue,
		"renewal": acceptedRenewalJSON, "previous_period_end": textValue, "accepted_at": textValue,
		"payment_method_id": uuidValue, "instrument": frozenInstrumentJSON,
		"hyperswitch": object(map[string]jsonRule{"account_id": textValue, "profile_id": textValue, "api_base_url": textValue}),
		"attempt":     integerValue, "failure_count": integerValue, "amount_minor": moneyStringValue, "order_reference": uuidValue,
	}),
	"provider_intents.subscription_collection.result_evidence": nullable(object(map[string]jsonRule{
		"stripe_recurring_decline": object(map[string]jsonRule{"decline_code": textValue, "binding": receiptBindingJSON, "failure_code": textValue, "payment_intent_id": textValue}),
		"failure_code":             textValue, "stripe_payment_intent_id": textValue, "authentication_required": booleanValue,
		"qualified_receipt": collectedReceiptJSON, "transaction_id": textValue, "rail": textValue, "verified_existing": booleanValue,
		"submitted_at": textValue, "declined": booleanValue, "response_code": integerValue, "not_executed": booleanValue, "not_executed_code": textValue,
		"operator_resolution":            operatorResolutionJSON,
		"collection_candidate":           object(map[string]jsonRule{"binding": receiptBindingJSON, "transaction_id": textValue, "external_invoice_id": textValue}),
		"rebill_decline":                 object(map[string]jsonRule{"binding": receiptBindingJSON, "response_code": integerValue, "provider_reference": textValue}),
		"qualified_invoice_nonexecution": object(map[string]jsonRule{"binding": receiptBindingJSON, "submitted_at": textValue, "code": textValue, "reason": textValue}),
	})),
	"provider_intents.manual_rebill.payload": object(map[string]jsonRule{
		"initiator": textValue, "requested_payment_method_id": uuidValue,
		"renewal":           acceptedRenewalJSON,
		"payment_method_id": uuidValue, "rail": textValue, "rail_subscription_id": textValue, "order_reference": uuidValue,
		"attempt": integerValue, "failure_count": integerValue, "amount_minor": moneyStringValue,
		"instrument": frozenInstrumentJSON,
	}),
	"provider_intents.manual_rebill.result_evidence": nullable(object(map[string]jsonRule{
		"qualified_receipt": collectedReceiptJSON, "transaction_id": textValue, "rail": textValue, "verified_existing": booleanValue,
		"declined": booleanValue, "response_code": integerValue, "not_executed": booleanValue, "submitted_at": textValue,
		"operator_resolution": operatorResolutionJSON,
		"rebill_preparation": object(map[string]jsonRule{
			"binding": receiptBindingJSON, "subscription_id": textValue, "customer_vault_id": textValue, "amount": textValue, "next_billing_date": textValue,
			"plan": object(map[string]jsonRule{"object": textValue, "id": textValue, "plan_name": textValue, "plan_amount": textValue, "plan_payments": textValue, "day_frequency": textValue, "month_frequency": textValue, "day_of_month": textValue}),
		}),
		"rebill_decline": object(map[string]jsonRule{"binding": receiptBindingJSON, "response_code": integerValue, "provider_reference": textValue}),
	})),
	"payments.metadata":          metadataJSON,
	"payments.discount_metadata": metadataJSON,
	"payment_methods.metadata":   metadataJSON,
	"usage_events.metadata":      metadataJSON,
	"invoice_items.metadata":     metadataJSON,
	// Successful checkout intents prune their submission payloads. Nonempty
	// payloads remain unqualified; retain only the exact typed replay results.
	"provider_intents.nmi_sale.payload": object(map[string]jsonRule{
		"checkout_attempt_id": uuidValue,
		"request_fingerprint": sha256Value, "provider": textValue, "psp": textValue, "amount": moneyStringValue, "currency": textValue, "description": textValue, "user_id": uuidValue, "price_id": uuidValue, "e2e_run_id": textValue,
		"credit_grant":      creditGrantJSON,
		"payment_method_id": uuidValue, "payment_id": uuidValue, "product_id": uuidValue, "list_amount": moneyStringValue, "accepted_at": textValue, "entitlements": acceptedEntitlementsJSON, "legacy_entitlements": dictionary(nullable(integerValue)), "access_duration_hours": nullable(integerValue), "entitlement_start": textValue, "ownership_start": textValue, "ownership_end": nullable(textValue), "eligibility": textValue,
		"instrument": frozenInstrumentJSON,
	}),
	"provider_intents.nmi_sale.result_evidence": nullable(object(map[string]jsonRule{
		"stripe_payment_intent_id": textValue, "authentication_required": booleanValue, "decline_code": textValue,
		"qualified_receipt": collectedReceiptJSON, "sale_submitted": booleanValue, "transaction_id": textValue, "payment_id": uuidValue, "delayed_start": textValue,
		"declined": booleanValue, "not_executed": booleanValue, "request_refused": booleanValue, "response_code": integerValue, "localization_id": textValue, "operator_resolution": operatorResolutionJSON,
		"duplicate_refused": booleanValue, "failure_code": textValue, "resolved_absent": booleanValue,
	})),
	"provider_intents.initial_membership.payload": object(map[string]jsonRule{
		"checkout_attempt_id": uuidValue,
		"terms":               initialMembershipTermsJSON,
		"instrument":          object(map[string]jsonRule{"psp_id": uuidValue, "custodian": textValue, "custodian_id": uuidValue, "rail_customer_ref": textValue, "rail_method_ref": textValue, "mandate": mandateJSON}),
		"native_schedule":     object(map[string]jsonRule{"plan_id": textValue, "start_date": textValue, "day_frequency": integerValue, "plan_payments": integerValue, "card": object(map[string]jsonRule{"FirstName": textValue, "LastName": textValue, "Address1": textValue, "City": textValue, "State": textValue, "Zip": textValue, "Country": textValue})}),
		"hyperswitch":         object(map[string]jsonRule{"account_id": textValue, "profile_id": textValue, "api_base_url": textValue}),
		"request_fingerprint": sha256Value, "checkout_idempotency_key": textValue, "psp": textValue, "email": textValue, "e2e_run_id": textValue, "requested_price": textValue,
	}),
	"provider_intents.initial_membership.result_evidence": nullable(object(map[string]jsonRule{
		"stripe_payment_intent_id": textValue, "authentication_required": booleanValue,
		"qualified_initial_refusal": object(map[string]jsonRule{"binding": receiptBindingJSON, "kind": textValue, "response_code": integerValue, "localization_id": textValue, "stripe_payment_intent_id": textValue, "stripe_failure_code": textValue, "stripe_decline_code": textValue}),
		"qualified_receipt":         collectedReceiptJSON, "initial_submitted": booleanValue, "not_executed": booleanValue, "request_refused": booleanValue, "operator_resolution": operatorResolutionJSON,
		"qualified_enrollment": object(map[string]jsonRule{"binding": receiptBindingJSON, "facts": object(map[string]jsonRule{
			"vault_billing_id": textValue, "order_reference": textValue, "po_number": textValue, "next_charge_date": textValue,
			"subscription": object(map[string]jsonRule{"object": textValue, "id": textValue, "start_date": textValue, "next_billing_date": textValue, "amount": textValue, "customer_vault_id": textValue, "delayed_condition": textValue, "paused_subscription": nullable(func(v any) bool { return textValue(v) || booleanValue(v) || integerValue(v) }), "plan": object(map[string]jsonRule{"object": textValue, "id": textValue, "plan_name": textValue, "plan_amount": textValue, "plan_payments": textValue, "day_frequency": textValue, "month_frequency": textValue, "day_of_month": textValue})}),
		})}),
		"candidate_subscription_ids": array(textValue), "transaction_id": textValue, "subscription_id": uuidValue, "status": textValue, "message": textValue, "delayed_start": textValue, "verified_existing": booleanValue,
		"declined": booleanValue, "response_code": integerValue, "localization_id": textValue, "provider_subscription_id": textValue,
	})),

	"custodians.settings":                        object(map[string]jsonRule{"public_api_key": textValue, "profile_id": textValue, "network_tokens": booleanSetting, "account_updater": booleanSetting, "account_updater_lookahead_days": integerSetting}),
	"merchant_configuration_applications.result": object(map[string]jsonRule{"application_id": textValue, "revision": textValue, "replayed": booleanValue}),
	"catalog_applications.result":                object(map[string]jsonRule{"application_id": textValue, "catalog_id": textValue, "base_revision": integerValue, "applied_revision": integerValue, "replayed": booleanValue, "products_changed": integerValue, "prices_changed": integerValue}),
	"products.entitlements":                      array(textValue),
	"subscriptions.entitlements_snapshot":        nullable(array(textValue)),
	// The model's legacy gateway_response column stores arbitrary subscription metadata.
	"subscriptions.gateway_response":    metadataJSON,
	"payments.entitlements_snapshot":    nullable(array(textValue)),
	"payments.legacy_entitlement_hours": nullable(dictionary(integerValue)),
	"billing_policies.policy": object(map[string]jsonRule{
		"kind": textValue, "outstanding_cap_amount": integerValue, "spend_windows": array(budgetWindow), "bad_spend_windows": array(budgetWindow), "accrual_rate_cap_per_hour": integerValue, "accrual_rate_window_seconds": integerValue, "collection_threshold_amount": nullable(integerValue), "collection_cycle_boundary": func(v any) bool { return v == "" }, "delinquency_grace_days": nullable(integerValue), "delinquency_amount_floor": nullable(integerValue), "policy_currency": textValue,
	}),
	"catalog_meters.group_by": nullable(dictionary(textValue)),
	"merchant_configurations.config": object(map[string]jsonRule{
		"profile": profileJSON, "collection_threshold": nullable(integerValue), "monthly_floor": nullable(integerValue), "billing_period_boundary": textValue, "arrears_grace_days": nullable(integerValue), "arrears_delinquency_floor": nullable(integerValue), "delegated_invoker_wasted_spend_windows": array(budgetWindow), "alert_email": textValue, "reprice_notice_window_days": nullable(integerValue), "renewal_receipt_min_interval_hours": nullable(integerValue), "provider_refund_access": textValue,
		"checkout_routing": array(object(map[string]jsonRule{"match": object(map[string]jsonRule{"currency": textValue, "product": textValue, "price": textValue, "mode": textValue, "country": textValue}), "prefer": array(textValue)})),
		"dunning_policy":   object(map[string]jsonRule{"tiers": array(object(map[string]jsonRule{"max_cycle_hours": integerValue, "retry_after_hours": array(integerValue)})), "transient_retry_minutes": array(integerValue), "access_during_dunning": textValue}),
	}),
	"psps.settings":                    pspSettingsJSON,
	"psps.signer":                      nullable(object(map[string]jsonRule{"mode": textValue, "key": textValue})),
	"price_psp_bindings.configuration": priceBindingConfigurationJSON,
	"catalog_rate_cards.filter":        nullable(dictionary(array(textValue))),
	"catalog_rate_cards.allowance":     nullable(object(map[string]jsonRule{"included": integerValue, "accrue_from": textValue, "cap": textValue})),
	"catalog_rate_cards.price":         rateJSON,
	"invoices.line_items":              invoiceLineJSON, "invoices.money_movements": dictionary(integerValue), "invoices.tax": emptyObject, "invoices.billing_contacts": contactsJSON,
	"customer_invoice_profiles.tax": emptyObject, "customer_invoice_profiles.billing_contacts": contactsJSON,
	"invoker_spend_limits.windows":  array(budgetWindow),
	"grants.spec_snapshot":          nullable(object(map[string]jsonRule{"entitlements": array(textValue), "deposit": object(map[string]jsonRule{"source": textValue, "invoker": textValue, "paid_amount": moneyStringValue})})),
	"usage_events.dimensions":       dictionary(integerValue),
	"checkout_attempts.metadata":    metadataJSON,
	"checkout_attempts.rail_fields": nullable(object(map[string]jsonRule{"rail": textValue, "psp": textValue, "payment_method_id": textValue, "token_symbol": textValue, "flow": textValue, "wallet": textValue, "email": textValue, "name_on_card": textValue, "first_name": textValue, "last_name": textValue, "address1": textValue, "city": textValue, "state": textValue, "zip": textValue, "country": textValue})),
	"checkout_attempts.rail_state": nullable(object(map[string]jsonRule{"initial_membership_quote": func(v any) bool {
		raw, ok := v.(string)
		if !ok {
			return false
		}
		var decoded any
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		return d.Decode(&decoded) == nil && d.Decode(new(any)) == io.EOF && initialMembershipTermsJSON(decoded)
	}, "accepted_purchase": acceptedPurchaseJSON, "customer_selected": booleanValue, "purchase_submitted": booleanValue, "provider_closed": booleanValue, "requested_entitlement": textValue, "requested_offer_kind": func(v any) bool { return v == "permanent" || v == "finite" || v == "recurring" }, "kind": textValue, "customer_ref": textValue, "consent": textValue, "payment_method_id": uuidValue, "capture": captureJSON, "_openrails_request_fingerprint": sha256Value, "subscription_id": textValue, "message": textValue, "failure_reason": textValue, "failure_code": textValue})),
	"checkout_attempts.routing_reason":   nullable(object(map[string]jsonRule{"policy": textValue, "rule": integerValue, "selected": textValue, "rail": textValue, "fallbacks": array(textValue), "skipped": array(object(map[string]jsonRule{"selector": textValue, "reason": textValue}))})),
	"host_outbox.data":                   object(map[string]jsonRule{"customer_id": textValue, "currency": textValue, "state": textValue, "overdue_started_at": textValue, "overdue_amount": integerValue, "overdue_invoices": integerValue, "entered_at": textValue, "evaluated_at": textValue}),
	"provider_intents.payload":           nullable(object(map[string]jsonRule{"original_payment_id": textValue, "reservation_id": textValue, "amount_cents": integerValue, "currency": textValue, "reason": textValue, "revoke_access": booleanValue, "provider_target": textValue, "provider_transaction_id": textValue})),
	"provider_intents.result_evidence":   nullable(object(map[string]jsonRule{"transaction_id": textValue, "response_code": integerValue, "retokenize": booleanValue, "verified_absent": booleanValue, "object_id": textValue, "already_inactive": booleanValue, "archived": booleanValue, "verified_inactive": booleanValue, "plan_pda": textValue, "already_sunset": booleanValue, "sunset": booleanValue, "signature": textValue, "verified_sunset": booleanValue})),
	"admission_operations.terms":         object(map[string]jsonRule{"invoker": textValue, "invoker_type": textValue, "trust_level": textValue, "roles": array(textValue), "resource": textValue, "source": textValue, "accrual_rate_delta_per_hour": integerValue}),
	"admission_operations.capture_terms": nullable(object(map[string]jsonRule{"event_type": textValue, "resource": textValue, "metadata": metadataJSON, "source": textValue, "source_id": textValue, "dimensions": dictionary(integerValue)})),
}

func validateJSON(field, raw string) error {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	v, err := readJSON(d, 0)
	if err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	r, ok := jsonRules[field]
	// Recovery holds are dispatcher state shared by operation kinds, never
	// provider proof. All remaining evidence still obeys its exact kind rule.
	if strings.HasPrefix(field, "provider_intents.") && strings.HasSuffix(field, ".result_evidence") {
		if evidence, isObject := v.(map[string]any); isObject {
			if held, present := evidence["recovery_held"]; present {
				if !booleanValue(held) {
					return fmt.Errorf("invalid recovery hold")
				}
				delete(evidence, "recovery_held")
			}
		}
	}
	if !ok || !r(v) {
		return fmt.Errorf("unsupported JSON contract")
	}
	return nil
}

func readJSON(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("JSON too deep")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				key, ok := k.(string)
				if !ok {
					return nil, fmt.Errorf("invalid key")
				}
				if _, dup := m[key]; dup {
					return nil, fmt.Errorf("duplicate key")
				}
				v, err := readJSON(d, depth+1)
				if err != nil {
					return nil, err
				}
				m[key] = v
			}
			_, err = d.Token()
			return m, err
		case '[':
			a := []any{}
			for d.More() {
				v, err := readJSON(d, depth+1)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			_, err = d.Token()
			return a, err
		default:
			return nil, fmt.Errorf("invalid delimiter")
		}
	}
	return t, nil
}

func safeText(s string) bool {
	if strings.Contains(s, "-----BEGIN ") || strings.Contains(s, "sk_live_") || strings.Contains(s, "sk_test_") {
		return false
	}
	// One card detector for the whole product (internal/cardguard). The archive
	// used to carry its own Luhn scan, which read a UUID's dashes as card
	// formatting and therefore needed shape exemptions to stay usable — the
	// exemptions are what made it diverge. It can conservatively refuse a
	// numeric provider handle; it never exports one.
	return !cardguard.ContainsPAN(s)
}
