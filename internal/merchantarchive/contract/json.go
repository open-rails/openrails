package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/cardguard"
)

type jsonRule func(any) bool

func textValue(v any) bool { s, ok := v.(string); return ok && safeText(s) }
func uuidValue(v any) bool { s, ok := v.(string); return ok && uuidPattern.MatchString(s) }
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
var budgetWindow = object(map[string]jsonRule{"key": textValue, "window_seconds": integerValue, "limit": integerValue, "currency": textValue})
var profileJSON = object(map[string]jsonRule{"display_name": textValue, "logo_url": textValue, "from_email": textValue, "support_url": textValue, "signup_url": textValue})
var contactsJSON = array(object(map[string]jsonRule{"name": textValue, "email": textValue}))
var operatorResolutionJSON = object(map[string]jsonRule{"actor": textValue, "reason": textValue, "resolved_at": textValue, "step": textValue, "not_executed": booleanValue, "provider_reference": textValue})
var invoiceLineJSON = array(object(map[string]jsonRule{"event_type": textValue, "amount": integerValue, "count": integerValue, "dimensions": dictionary(integerValue)}))
var pspSettingsJSON = object(map[string]jsonRule{"tokenization_key": textValue, "tokenization_url": textValue, "rpc_provider": textValue, "recipient_wallet": textValue, "tokens": dictionary(object(map[string]jsonRule{"mint": textValue, "name": textValue}))})
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
	"version": integerValue, "family": textValue,
	"binding": object(map[string]jsonRule{"operation_id": uuidValue, "merchant_id": uuidValue, "psp_id": uuidValue, "kind": textValue, "payload_sha256": textValue}),
	"nmi":     object(map[string]jsonRule{"transaction_id": textValue, "order_reference": textValue, "customer_vault_id": textValue, "amount": moneyStringValue, "currency": textValue, "approved": booleanValue}),
	"stripe":  object(map[string]jsonRule{"invoice_id": textValue, "status": textValue, "customer_id": textValue, "payment_method_id": textValue, "amount_paid": moneyStringValue, "currency": textValue, "charge_id": textValue, "payment_intent_id": textValue, "collection_key": textValue, "charged_amount": moneyStringValue, "charge_currency": textValue, "charge_customer_id": textValue, "charge_paid": booleanValue, "charge_captured": booleanValue, "charge_status": textValue, "charge_invoice_id": textValue, "charge_payment_intent_id": textValue}),
})

var receiptBindingJSON = object(map[string]jsonRule{"operation_id": uuidValue, "merchant_id": uuidValue, "psp_id": uuidValue, "kind": textValue, "payload_sha256": textValue})

// Exact nested shapes keep raw metadata/provider bodies out of the archive.
// An unsupported shape is a refusal, never a lossy rewrite of a replay body.
var jsonRules = map[string]jsonRule{
	// The completed collection operation retains its frozen instrument and
	// accepted terms for replay; none of these fields contains card data.
	"rail_intents.invoice_collection.payload": object(map[string]jsonRule{
		"initiator":  textValue,
		"invoice_id": uuidValue, "customer_id": uuidValue, "attempt_id": uuidValue, "payment_method_id": uuidValue,
		"rail": textValue, "currency": textValue, "amount": integerValue, "amount_minor": integerValue, "description": textValue, "provider_customer_ref": textValue,
		"instrument": object(map[string]jsonRule{"psp_id": uuidValue, "custodian": textValue, "custodian_id": uuidValue, "rail_customer_ref": textValue, "rail_method_ref": textValue, "stored_credential_recurring_ref": textValue, "stored_credential_unscheduled_ref": textValue}),
	}),
	"rail_intents.invoice_collection.result_evidence": nullable(object(map[string]jsonRule{
		"qualified_receipt": collectedReceiptJSON,

		"transaction_id": textValue, "external_invoice_id": textValue, "rail": textValue,
		"declined": booleanValue, "failure_code": textValue, "failure_message": textValue, "not_executed": booleanValue,
		"not_executed_code": textValue, "submitted_at": textValue, "provider_contradiction": textValue, "verified_existing": booleanValue,
		"operator_resolution": operatorResolutionJSON,
	})),
	"rail_intents.manual_rebill.payload": object(map[string]jsonRule{
		"initiator": textValue, "requested_payment_method_id": uuidValue,
		"renewal": object(map[string]jsonRule{
			"psp_id": uuidValue, "subscription_id": uuidValue, "customer_id": uuidValue,
			"from_price_id": uuidValue, "from_product_id": uuidValue, "price_id": uuidValue, "product_id": uuidValue,
			"product_name": textValue, "amount": moneyStringValue, "currency": textValue,
			"period_start": textValue, "period_end": textValue,
			"entitlements": nullable(dictionary(nullable(integerValue))), "previous_entitlements": nullable(dictionary(nullable(integerValue))),
			"reprice_id": uuidValue, "scheduled_price_id": uuidValue,
		}),
		"payment_method_id": uuidValue, "rail": textValue, "rail_subscription_id": textValue, "order_reference": uuidValue,
		"attempt": integerValue, "failure_count": integerValue, "amount_minor": moneyStringValue,
		"instrument": object(map[string]jsonRule{"psp_id": uuidValue, "custodian": textValue, "custodian_id": uuidValue, "rail_customer_ref": textValue, "rail_method_ref": textValue, "stored_credential_recurring_ref": textValue, "stored_credential_unscheduled_ref": textValue}),
	}),
	"rail_intents.manual_rebill.result_evidence": nullable(object(map[string]jsonRule{
		"qualified_receipt": collectedReceiptJSON, "transaction_id": textValue, "rail": textValue, "verified_existing": booleanValue,
		"declined": booleanValue, "response_code": integerValue, "not_executed": booleanValue, "submitted_at": textValue,
		"operator_resolution": operatorResolutionJSON,
		"rebill_preparation": object(map[string]jsonRule{
			"binding": receiptBindingJSON, "subscription_id": textValue, "customer_vault_id": textValue, "amount": textValue, "next_billing_date": textValue,
			"plan": object(map[string]jsonRule{"object": textValue, "id": textValue, "plan_name": textValue, "plan_amount": textValue, "plan_payments": textValue, "day_frequency": textValue, "month_frequency": textValue, "day_of_month": textValue}),
		}),
		"rebill_decline": object(map[string]jsonRule{"binding": receiptBindingJSON, "response_code": integerValue, "provider_reference": textValue}),
	})),
	// Engine-authored payment correlation, not an arbitrary provider body.
	"payments.metadata": nullable(object(map[string]jsonRule{
		"order_id": textValue, "provider_transaction_id": textValue, "e2e_run_id": textValue, "stripe_invoice_id": textValue,
		"refund_review": func(v any) bool { return v == "confirmed charge on a cancelled subscription" },
	})),
	"invoice_items.metadata": nullable(object(map[string]jsonRule{
		"operation": textValue, "source": textValue,
	})),
	// Successful checkout intents prune their submission payloads. Nonempty
	// payloads remain unqualified; retain only the exact typed replay results.
	"rail_intents.nmi_sale.payload": nullable(emptyObject),
	"rail_intents.nmi_sale.result_evidence": nullable(object(map[string]jsonRule{
		"transaction_id": textValue, "payment_id": uuidValue, "delayed_start": textValue, "verified_existing": booleanValue,
		"declined": booleanValue, "response_code": integerValue, "localization_id": textValue,
	})),
	"rail_intents.nmi_subscription_create.payload": nullable(emptyObject),
	"rail_intents.nmi_subscription_create.result_evidence": nullable(object(map[string]jsonRule{
		"transaction_id": textValue, "subscription_id": uuidValue, "status": textValue, "message": textValue, "delayed_start": textValue, "verified_existing": booleanValue,
		"declined": booleanValue, "response_code": integerValue, "localization_id": textValue, "provider_subscription_id": textValue,
	})),

	"custodians.settings":                      object(map[string]jsonRule{"public_api_key": textValue, "network_tokens": booleanSetting, "account_updater": booleanSetting, "account_updater_lookahead_days": integerSetting}),
	"products.entitlements_spec":               nullable(dictionary(nullable(integerValue))),
	"subscriptions.entitlements_spec_snapshot": nullable(dictionary(nullable(integerValue))),
	// Checkout writes correlation coordinates and delayed-start metadata;
	// ordinary subscription updates add notes and supersession markers.
	// Superseding a NULL response wraps it as previous_gateway_response:null.
	"subscriptions.gateway_response": nullable(object(map[string]jsonRule{
		"order_id": textValue, "provider_transaction_id": textValue,
		"delayed_start": textValue, "e2e_run_id": textValue, "admin_notes": textValue,
		"superseded_at": textValue, "superseded_by_subscription_id": nullable(textValue),
		"previous_gateway_response": func(v any) bool { return v == nil },
	})),
	"payments.entitlements_spec_snapshot": nullable(dictionary(nullable(integerValue))),
	"billing_policies.policy": object(map[string]jsonRule{
		"kind": textValue, "outstanding_cap_amount": integerValue, "spend_windows": array(budgetWindow), "bad_spend_windows": array(budgetWindow), "accrual_rate_cap_per_hour": integerValue, "accrual_rate_window_seconds": integerValue, "collection_threshold_amount": nullable(integerValue), "collection_cycle_boundary": func(v any) bool { return v == "" }, "delinquency_grace_days": nullable(integerValue), "delinquency_amount_floor": nullable(integerValue), "policy_currency": textValue,
	}),
	"catalog_meters.group_by": nullable(dictionary(textValue)),
	"merchant_configurations.config": object(map[string]jsonRule{
		"profile": profileJSON, "collection_threshold": nullable(integerValue), "monthly_floor": nullable(integerValue), "billing_period_boundary": textValue, "arrears_grace_days": nullable(integerValue), "arrears_delinquency_floor": nullable(integerValue), "delegated_invoker_wasted_spend_windows": array(budgetWindow), "alert_email": textValue, "reprice_notice_window_days": nullable(integerValue),
		"checkout_routing": array(object(map[string]jsonRule{"match": object(map[string]jsonRule{"currency": textValue, "product": textValue, "price": textValue, "mode": textValue, "country": textValue}), "prefer": array(textValue)})),
	}),
	"psps.evidence":                    object(map[string]jsonRule{"settings": pspSettingsJSON, "public_config": object(map[string]jsonRule{"publishable_key": textValue, "tokenization_key": textValue}), "signer": object(map[string]jsonRule{"mode": textValue, "key": textValue})}),
	"price_psp_bindings.configuration": emptyObject,
	"catalog_rate_cards.filter":        nullable(dictionary(array(textValue))),
	"catalog_rate_cards.allowance":     nullable(object(map[string]jsonRule{"included": integerValue, "accrue_from": textValue, "cap": textValue})),
	"catalog_rate_cards.price":         rateJSON,
	"invoices.line_items":              invoiceLineJSON, "invoices.money_movements": dictionary(integerValue), "invoices.tax": emptyObject, "invoices.billing_contacts": contactsJSON,
	"customer_invoice_profiles.tax": emptyObject, "customer_invoice_profiles.billing_contacts": contactsJSON,
	"invoker_spend_limits.windows":       array(budgetWindow),
	"grants.spec_snapshot":               nullable(object(map[string]jsonRule{"entitlements": array(textValue), "deposit": object(map[string]jsonRule{"source": textValue, "invoker": textValue})})),
	"usage_events.dimensions":            dictionary(integerValue),
	"checkout_sessions.metadata":         nullable(emptyObject),
	"checkout_sessions.rail_fields":      nullable(object(map[string]jsonRule{"rail": textValue, "psp": textValue, "payment_method_id": textValue, "token_symbol": textValue, "flow": textValue, "wallet": textValue, "email": textValue, "name_on_card": textValue, "first_name": textValue, "last_name": textValue, "address1": textValue, "city": textValue, "state": textValue, "zip": textValue, "country": textValue})),
	"checkout_sessions.rail_state":       nullable(object(map[string]jsonRule{"_openrails_request_fingerprint": textValue, "subscription_id": textValue, "message": textValue, "failure_reason": textValue, "failure_code": textValue})),
	"checkout_sessions.routing_reason":   nullable(object(map[string]jsonRule{"policy": textValue, "rule": integerValue, "selected": textValue, "rail": textValue, "fallbacks": array(textValue), "skipped": array(object(map[string]jsonRule{"selector": textValue, "reason": textValue}))})),
	"host_outbox.data":                   object(map[string]jsonRule{"customer_id": textValue, "currency": textValue, "state": textValue, "overdue_since": textValue, "overdue_amount": integerValue, "overdue_invoices": integerValue, "entered_at": textValue, "evaluated_at": textValue}),
	"rail_intents.payload":               nullable(object(map[string]jsonRule{"original_payment_id": textValue, "reservation_id": textValue, "amount_cents": integerValue, "currency": textValue, "reason": textValue, "revoke_access": booleanValue, "provider_target": textValue, "provider_transaction_id": textValue})),
	"rail_intents.result_evidence":       nullable(object(map[string]jsonRule{"transaction_id": textValue, "response_code": integerValue, "retokenize": booleanValue, "verified_absent": booleanValue, "object_id": textValue, "already_inactive": booleanValue, "archived": booleanValue, "verified_inactive": booleanValue, "plan_pda": textValue, "already_sunset": booleanValue, "sunset": booleanValue, "signature": textValue, "verified_sunset": booleanValue})),
	"admission_operations.terms":         object(map[string]jsonRule{"invoker": textValue, "invoker_type": textValue, "trust_level": textValue, "roles": array(textValue), "resource": textValue, "source": textValue, "accrual_rate_delta_per_hour": integerValue}),
	"admission_operations.capture_terms": nullable(object(map[string]jsonRule{"event_type": textValue, "resource": textValue, "metadata": emptyObject, "source": textValue, "source_id": textValue, "dimensions": dictionary(integerValue)})),
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
