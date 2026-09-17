package format

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// These fields are typed billing contracts, not arbitrary provider payloads.
// Unknown top-level keys refuse the archive rather than smuggling a new field
// into the destination. Nested objects are additionally checked for credentials.
var jsonKeys = map[string]string{
	"custodians.settings":                "public_api_key network_tokens",
	"psps.evidence":                      "settings signer",
	"price_psp_bindings.configuration":   "",
	"merchant_configurations.config":     "profile collection_threshold monthly_floor billing_period_boundary arrears_grace_days arrears_delinquency_floor delegated_invoker_wasted_spend_windows alert_email reprice_notice_window_days checkout_routing",
	"billing_policies.policy":            "kind outstanding_cap_amount spend_windows accrual_rate_cap_per_hour accrual_rate_window_seconds bad_spend_windows collection_threshold_amount delinquency_grace_days delinquency_amount_floor policy_currency",
	"catalog_rate_cards.price":           "model currency flat per_unit tiered package maximum_amount matrix",
	"catalog_rate_cards.allowance":       "included accrue_from cap",
	"grants.spec_snapshot":               "entitlements deposit",
	"invoices.tax":                       "",
	"customer_invoice_profiles.tax":      "",
	"rail_intents.payload":               "original_payment_id reservation_id amount_cents reason revoke_access provider_target provider_transaction_id",
	"rail_intents.result_evidence":       "transaction_id response_code retokenize verified_absent object_id already_inactive archived verified_inactive plan_pda already_sunset sunset signature verified_sunset",
	"admission_operations.terms":         "invoker invoker_type trust_level roles resource source accrual_rate_delta_per_hour",
	"admission_operations.capture_terms": "amount actual_amount final_amount source source_id resource invoker invoker_type dimensions event_type pricing_authority",
}

func ValidateValues(p Profile, values []*string) error {
	if len(values) != len(p.Columns) {
		return fmt.Errorf("invalid row width for %s", p.Name)
	}
	for i, c := range p.Columns {
		if values[i] == nil {
			continue
		}
		v := *values[i]
		bad := func() error { return fmt.Errorf("unsupported or invalid value in %s.%s", p.Name, c.Name) }
		if c.Name == "currency" && strings.HasPrefix(v, "credit:") {
			return bad()
		}
		switch c.Type {
		case "uuid":
			if !uuidPattern.MatchString(v) {
				return bad()
			}
		case "bigint", "integer":
			bits := 64
			if c.Type == "integer" {
				bits = 32
			}
			x, err := strconv.ParseInt(v, 10, bits)
			if err != nil || strconv.FormatInt(x, 10) != v {
				return bad()
			}
		case "boolean":
			if v != "true" && v != "false" {
				return bad()
			}
		case "jsonb":
			if err := validateJSON(p.Name+"."+c.Name, v); err != nil {
				return bad()
			}
		}
		switch p.Name + "." + c.Name {
		case "payments.status":
			if v == "pending" {
				return bad()
			}
		case "invoice_payments.status":
			if v == "attempted" {
				return bad()
			}
		case "admission_operations.state":
			if v != "released" && v != "captured" {
				return bad()
			}
		case "rail_intents.status":
			if v != "succeeded" && v != "failed_terminal" && v != "expired" && v != "superseded" {
				return bad()
			}
		case "maintenance_runs.kind":
			if v != "prune" && v != "converge_enforce" && v != "merchant_purge" {
				return bad()
			}
		case "maintenance_runs.status":
			if v == "running" {
				return bad()
			}
		}
	}
	if p.Name == "webhook_events" && value(p, values, "completed_at") == nil {
		return fmt.Errorf("unfinished webhook event")
	}
	if p.Name == "rail_intents" {
		typ := value(p, values, "intent_type")
		payload := value(p, values, "payload")
		if payload != nil && *payload != "null" && *payload != "{}" && (typ == nil || (*typ != "nmi_refund" && *typ != "stripe_refund" && *typ != "ccbill_refund")) {
			return fmt.Errorf("unsupported retained intent payload")
		}
	}
	return nil
}

func value(p Profile, values []*string, name string) *string {
	for i, c := range p.Columns {
		if c.Name == name {
			return values[i]
		}
	}
	return nil
}

func validateJSON(field, raw string) error {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	if keys, exists := jsonKeys[field]; exists && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object")
		}
		for k := range m {
			if !strings.Contains(" "+keys+" ", " "+k+" ") {
				return fmt.Errorf("unknown contract key")
			}
		}
	}
	if strings.HasSuffix(field, "entitlements_spec") || strings.HasSuffix(field, "entitlements_spec_snapshot") || field == "invoices.money_movements" || field == "usage_events.dimensions" {
		if v != nil {
			m, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("expected measurement map")
			}
			for _, n := range m {
				if _, ok := n.(json.Number); !ok {
					return fmt.Errorf("expected numeric measurement")
				}
			}
		}
	}
	return credentialFree(v, field, 0)
}

func credentialFree(v any, field string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("JSON too deep")
	}
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			n := strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(k))
			if n != "public_api_key" && (strings.Contains(n, "secret") || strings.Contains(n, "password") || strings.Contains(n, "credential") || strings.Contains(n, "private_key") || n == "pan" || n == "card_number" || n == "cvv" || n == "cvc" || n == "api_key" || n == "authorization" || n == "access_token" || n == "refresh_token") {
				return fmt.Errorf("credential field")
			}
			if err := credentialFree(v, field, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range x {
			if err := credentialFree(v, field, depth+1); err != nil {
				return err
			}
		}
	case string:
		if bytes.Contains([]byte(x), []byte("-----BEGIN ")) {
			return fmt.Errorf("key material")
		}
	}
	return nil
}
