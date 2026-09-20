package contract

import (
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"strconv"
	"strings"
	"time"
)

func ValidateValues(p Profile, values []*string) error {
	if len(values) != len(p.Columns) {
		return fmt.Errorf("invalid row width for %s", p.Name)
	}
	for i, c := range p.Columns {
		if values[i] == nil {
			continue
		}
		v := *values[i]
		if c.Type == "text" || strings.HasPrefix(c.Type, "character varying") {
			if !safeText(v) {
				return fmt.Errorf("sensitive text in %s.%s", p.Name, c.Name)
			}
		}
		// The settlement trigger derives this key from the payment row, and the
		// archive restores it verbatim, so it must still name its own payment on
		// its own event. This is a coordinate check, not a card exemption: the
		// card scan above runs on it like on any other text.
		if p.Name == "host_outbox" && c.Name == "dedupe_key" {
			event := value(p, values, "event_type")
			settlement := event != nil && *event == "payment.settled"
			if settlement || strings.HasPrefix(v, "payment:") {
				payment := value(p, values, "payment_id")
				if !settlement || payment == nil || !uuidPattern.MatchString(*payment) || v != "payment:"+*payment {
					return fmt.Errorf("invalid payment settlement dedupe key")
				}
			}
		}
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
		case "timestamp with time zone", "timestamptz":
			parsed, err := time.Parse("2006-01-02 15:04:05.999999-07", v)
			if err != nil || !strings.HasSuffix(v, "+00") || parsed.Format("2006-01-02 15:04:05.999999-07") != v {
				return bad()
			}
		case "text[]":
			if !safeTextArray(v) {
				return bad()
			}
		case "jsonb":
			field := p.Name + "." + c.Name
			if p.Name == "rail_intents" && (c.Name == "payload" || c.Name == "result_evidence") {
				if typ := value(p, values, "intent_type"); typ != nil && (*typ == "nmi_sale" || *typ == "nmi_subscription_create" || *typ == "nmi_provider_cutover" || *typ == "invoice_collection") {
					field = p.Name + "." + *typ + "." + c.Name
				}
			}
			if err := validateJSON(field, v); err != nil {
				return bad()
			}
		}
		switch p.Name + "." + c.Name {
		case "invoices.collection_intent_id":
			return bad() // Any live collection must resolve before cutover.
		case "payments.status":
			if v == "pending" {
				return bad()
			}
		case "invoice_payments.status":
			if v == "attempted" {
				return bad()
			}
		case "checkout_sessions.status":
			if v != "succeeded" && v != "failed" && v != "expired" && v != "canceled" {
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
	if p.Name == "host_outbox" && value(p, values, "delivered_at") == nil {
		return fmt.Errorf("undelivered host event")
	}
	if p.Name == "rail_intents" {
		typ := value(p, values, "intent_type")
		payload := value(p, values, "payload")
		if payload != nil && *payload != "null" && *payload != "{}" && (typ == nil || (*typ != "nmi_refund" && *typ != "stripe_refund" && *typ != "ccbill_refund" && *typ != "nmi_provider_cutover" && *typ != "invoice_collection")) {
			return fmt.Errorf("unsupported retained intent payload")
		}
		if typ != nil && (*typ == "nmi_refund" || *typ == "stripe_refund" || *typ == "ccbill_refund") {
			if payload == nil {
				return fmt.Errorf("refund operation has no accepted payload")
			}
			if _, err := intents.DecodeRefundPayload(gen.OpenrailsRailIntent{Payload: []byte(*payload)}); err != nil {
				return fmt.Errorf("invalid accepted refund payload: %w", err)
			}
		}
		if typ != nil && *typ == "invoice_collection" {
			field := func(name string) string {
				if v := value(p, values, name); v != nil {
					return *v
				}
				return ""
			}
			id, _ := uuid.Parse(field("id"))
			merchant, _ := uuid.Parse(field("merchant_id"))
			psp, _ := uuid.Parse(field("psp_id"))
			row := gen.OpenrailsRailIntent{ID: id, MerchantID: merchant, PspID: &psp, Rail: field("rail"), IntentType: *typ, Payload: []byte(field("payload")), ResultEvidence: []byte(field("result_evidence")), Status: field("status")}
			if _, err := intents.DecodeInvoiceCollectionPayload(row); err != nil {
				return fmt.Errorf("invalid accepted collection payload: %w", err)
			}
			_, found, err := intents.LoadCollectedReceipt(row)
			if err != nil {
				return fmt.Errorf("invalid qualified collection receipt: %w", err)
			}
			if (row.Status == intents.StatusSucceeded) != found {
				return fmt.Errorf("collection terminal state and qualified receipt disagree")
			}
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
