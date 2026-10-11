package contract

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func ValidateValues(p Profile, values []*string) error {
	if len(values) != len(p.Columns) {
		return fmt.Errorf("invalid row width for %s", p.Name)
	}
	for _, field := range []string{"entitlements", "entitlements_snapshot"} {
		if raw := value(p, values, field); raw != nil {
			var names []string
			if err := json.Unmarshal([]byte(*raw), &names); err != nil {
				return fmt.Errorf("invalid %s.%s: %w", p.Name, field, err)
			}
			if _, err := catalog.NormalizeEntitlements(names); err != nil {
				return fmt.Errorf("invalid %s.%s: %w", p.Name, field, err)
			}
		}
	}
	if p.Name == "payments" {
		if raw := value(p, values, "legacy_entitlement_hours"); raw != nil {
			var hours map[string]int
			if err := json.Unmarshal([]byte(*raw), &hours); err != nil {
				return err
			}
			var names []string
			if raw := value(p, values, "entitlements_snapshot"); raw != nil {
				if err := json.Unmarshal([]byte(*raw), &names); err != nil {
					return err
				}
			}
			for name, duration := range hours {
				if duration <= 0 || duration > catalog.MaxDurationHours || !slices.Contains(names, name) {
					return fmt.Errorf("invalid historical payment entitlement duration")
				}
			}
		}
	}
	if p.Name == "catalog_applications" {
		var receipt struct {
			ApplicationID   string `json:"application_id"`
			BaseRevision    int64  `json:"base_revision"`
			AppliedRevision int64  `json:"applied_revision"`
			Replayed        bool   `json:"replayed"`
			ProductsChanged int    `json:"products_changed"`
			PricesChanged   int    `json:"prices_changed"`
		}
		raw := value(p, values, "result")
		id := value(p, values, "application_id")
		base := value(p, values, "base_revision")
		applied := value(p, values, "applied_revision")
		digest := value(p, values, "request_sha256")
		if raw == nil || id == nil || base == nil || applied == nil || digest == nil || json.Unmarshal([]byte(*raw), &receipt) != nil {
			return fmt.Errorf("invalid catalog application receipt")
		}
		if receipt.ApplicationID != *id || strconv.FormatInt(receipt.BaseRevision, 10) != *base || strconv.FormatInt(receipt.AppliedRevision, 10) != *applied || receipt.BaseRevision < 0 || receipt.AppliedRevision != receipt.BaseRevision+1 || receipt.Replayed || receipt.ProductsChanged < 0 || receipt.PricesChanged < 0 {
			return fmt.Errorf("contradictory catalog application receipt")
		}
		if len(*digest) != 66 || !strings.HasPrefix(*digest, `\x`) {
			return fmt.Errorf("invalid catalog application digest")
		}
		if _, err := hex.DecodeString((*digest)[2:]); err != nil {
			return fmt.Errorf("invalid catalog application digest")
		}
	}
	if p.Name == "payments" {
		if raw := value(p, values, "credit_grant_snapshot"); raw != nil && *raw != "null" {
			var benefit models.CreditGrantSnapshot
			currency := value(p, values, "currency")
			status := value(p, values, "status")
			if json.Unmarshal([]byte(*raw), &benefit) != nil || benefit.Validate() != nil || currency == nil || benefit.Currency != *currency || status != nil && *status == "succeeded" && benefit.StartsAt.IsZero() {
				return fmt.Errorf("invalid purchased credit payment snapshot")
			}
		}
	}
	if p.Name == "subscriptions" {
		policy, rail, binding := value(p, values, "collection_policy"), value(p, values, "rail"), value(p, values, "rail_subscription_id")
		if policy == nil || !models.CollectionPolicy(*policy).Valid() || rail == nil {
			return fmt.Errorf("subscription lacks a valid collection policy or binding")
		}
		hasBinding := binding != nil && *binding != ""
		if (*policy == "nmi_schedule") != (*rail == "nmi" && *policy != "engine") {
			return fmt.Errorf("nmi_schedule is the policy of every provider-scheduled NMI subscription")
		}
		if *policy != "engine" && !hasBinding {
			return fmt.Errorf("provider collection lacks a schedule binding")
		}
		// Engine card subscriptions have no provider schedule: its absence is
		// stored as SQL NULL. Solana still binds the on-chain subscription.
		if *policy == "engine" && !((*rail == "nmi" || *rail == "stripe") && !hasBinding || *rail == "solana" && hasBinding) {
			return fmt.Errorf("engine collection has contradictory schedule binding")
		}
	}
	// Canonical collection operations carry typed engine-encoded keys.
	// Validate the accepted payer/authority/payload and retained custody before
	// treating that one coordinate as an identity rather than arbitrary text.
	encodedKey, err := validateRetainedPayment(p, values)
	if err != nil {
		return err
	}
	if p.Name == "ledger_transfers" {
		key, payer, order := value(p, values, "source_id"), value(p, values, "customer_id"), value(p, values, "order_id")
		if key != nil && intents.IsOrderCollectionKey(*key) {
			payerID, orderID := uuid.Nil, uuid.Nil
			if payer != nil {
				payerID, _ = uuid.Parse(*payer)
			}
			if order != nil {
				orderID, _ = uuid.Parse(*order)
			}
			encodedKey = orderID != uuid.Nil && intents.OrderCollectionKeyValid(payerID, orderID, *key)
			if encodedKey && p.Name == "ledger_transfers" {
				for field, expected := range map[string]string{"source": "invoice_charge", "operation": "invoice_payment", "transfer_type": "owed_payment"} {
					if actual := value(p, values, field); actual == nil || *actual != expected {
						return fmt.Errorf("invalid encoded collection ledger coordinate")
					}
				}
			}
			// Export/restore also require the exact canonical operation, attempt
			// and settlement coordinate; a hash-shaped orphan is not portable.
		}
	}

	for i, c := range p.Columns {
		if values[i] == nil {
			continue
		}
		v := *values[i]
		if c.Type == "text" {
			if !(encodedKey && (c.Name == "idempotency_key" || p.Name == "ledger_transfers" && c.Name == "source_id")) && !safeText(v) {
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
		case "bigint", "integer", "smallint":
			bits := map[string]int{"bigint": 64, "integer": 32, "smallint": 16}[c.Type]
			x, err := strconv.ParseInt(v, 10, bits)
			if err != nil || strconv.FormatInt(x, 10) != v {
				return bad()
			}
			switch p.Name + "." + c.Name {
			case "prices.access_duration_hours", "prices.billing_interval_hours", "prices.trial_duration_hours", "subscriptions.access_duration_hours_snapshot":
				if x <= 0 || x > int64(catalog.MaxDurationHours) {
					return bad()
				}
			case "subscriptions.quantity", "payments.quantity", "grants.quantity", "product_access.quantity":
				if x < 1 {
					return bad()
				}
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
			if p.Name == "provider_intents" && (c.Name == "payload" || c.Name == "result_evidence") {
				if typ := value(p, values, "intent_type"); typ != nil && (*typ == intents.TypeNMIPaymentMethodDelete || *typ == intents.TypeHyperSwitchMethodDelete || *typ == "nmi_sale" || *typ == "initial_membership" || *typ == intents.TypeOrderCollection || *typ == "manual_rebill" || *typ == subscriptions.TypeSubscriptionCollection) {
					field = p.Name + "." + *typ + "." + c.Name
				}
			}
			if err := validateJSON(field, v); err != nil {
				return bad()
			}
		}
		switch p.Name + "." + c.Name {
		case "payments.status":
			if v == "pending" {
				return bad()
			}
		case "checkout_attempts.status":
			if v != "succeeded" && v != "failed" && v != "expired" && v != "canceled" {
				return bad()
			}
		case "admission_operations.state":
			if v != "released" && v != "captured" {
				return bad()
			}
		case "provider_intents.status":
			if v != "succeeded" && v != "failed_terminal" && v != "expired" && v != "superseded" {
				return bad()
			}
		case "maintenance_runs.kind":
			if v != "prune" && v != "approval" && v != "merchant_purge" {
				return bad()
			}
		case "maintenance_runs.status":
			if v == "running" {
				return bad()
			}
		}
	}
	if p.Name == "payment_methods" {
		if raw := value(p, values, "capture"); raw != nil && *raw != "null" {
			var capture models.CheckoutCapture
			if json.Unmarshal([]byte(*raw), &capture) != nil {
				return fmt.Errorf("invalid card capture binding")
			}
			owner, customer := uuid.Nil, uuid.Nil
			if v := value(p, values, "merchant_id"); v != nil {
				owner, _ = uuid.Parse(*v)
			}
			if v := value(p, values, "customer_id"); v != nil {
				customer, _ = uuid.Parse(*v)
			}
			status := models.CheckoutAttemptStatusExpired
			if capture.PaymentMethodID != uuid.Nil {
				status = models.CheckoutAttemptStatusSucceeded
			}
			if _, err := models.DecodeCheckoutCapture([]byte(*raw), owner, customer, capture.PSPID, status, &capture.ExpiresAt); err != nil {
				return err
			}
		}
	}
	if p.Name == "checkout_attempts" {
		if id := value(p, values, "order_id"); id == nil || !uuidPattern.MatchString(*id) {
			return fmt.Errorf("checkout attempt requires its order")
		}
		if value(p, values, "amount") == nil || value(p, values, "currency") == nil {
			return fmt.Errorf("checkout attempt requires its monetary terms")
		}
	}

	if p.Name == "webhook_events" && value(p, values, "completed_at") == nil {
		return fmt.Errorf("unfinished webhook event")
	}
	if p.Name == "host_outbox" && value(p, values, "delivered_at") == nil {
		return fmt.Errorf("undelivered host event")
	}
	if p.Name == "provider_intents" {
		typ := value(p, values, "intent_type")
		payload := value(p, values, "payload")
		if payload != nil && *payload != "null" && *payload != "{}" && (typ == nil || (*typ != intents.TypeNMIPaymentMethodDelete && *typ != intents.TypeHyperSwitchMethodDelete && *typ != "nmi_refund" && *typ != "stripe_refund" && *typ != "ccbill_refund" && *typ != intents.TypeOrderCollection && *typ != "nmi_sale" && *typ != "initial_membership" && *typ != "manual_rebill" && *typ != subscriptions.TypeSubscriptionCollection)) {
			return fmt.Errorf("unsupported retained intent payload")
		}
		if typ != nil && (*typ == "nmi_refund" || *typ == "stripe_refund" || *typ == "ccbill_refund") {
			if payload == nil {
				return fmt.Errorf("refund operation has no accepted payload")
			}
			if _, err := intents.DecodeRefundPayload(gen.BillingProviderIntent{Payload: []byte(*payload)}); err != nil {
				return fmt.Errorf("invalid accepted refund payload: %w", err)
			}
		}

	}
	return nil
}

// validateRetainedPayment also identifies an engine-generated key. A caller key or arbitrary metadata
// never reaches this exception merely by resembling a hash.
func validateRetainedPayment(p Profile, values []*string) (bool, error) {
	if p.Name != "provider_intents" {
		return false, nil
	}
	field := func(name string) string {
		if v := value(p, values, name); v != nil {
			return *v
		}
		return ""
	}
	typ := field("intent_type")
	if typ != intents.TypeNMIPaymentMethodDelete && typ != intents.TypeHyperSwitchMethodDelete && typ != intents.TypeOrderCollection && typ != subscriptions.TypeManualRebill && typ != subscriptions.TypeSubscriptionCollection && typ != payments.TypeNMISale && typ != subscriptions.TypeInitialMembership {
		return false, nil
	}
	id, _ := uuid.Parse(field("id"))
	merchant, _ := uuid.Parse(field("merchant_id"))
	row := gen.BillingProviderIntent{ID: id, MerchantID: merchant, Rail: field("rail"), IntentType: typ, Payload: []byte(field("payload")), ResultEvidence: []byte(field("result_evidence")), Status: field("status"), Origin: field("origin"), IdempotencyKey: field("idempotency_key")}
	if actor := field("actor"); actor != "" {
		row.Actor = &actor
	}
	for name, target := range map[string]**uuid.UUID{"psp_id": &row.PspID, "subscription_id": &row.SubscriptionID, "price_id": &row.PriceID, "custodian_id": &row.CustodianID} {
		if v := value(p, values, name); v != nil {
			parsed, err := uuid.Parse(*v)
			if err != nil {
				return false, fmt.Errorf("invalid collection %s: %w", name, err)
			}
			*target = &parsed
		}
	}
	if typ == subscriptions.TypeInitialMembership {
		return false, intents.ValidateInitialMembershipTerminal(row)
	}
	if typ == intents.TypeHyperSwitchMethodDelete || typ == intents.TypeNMIPaymentMethodDelete {
		_, _, err := intents.DeletedMethod(row)
		return false, err
	}
	if typ == payments.TypeNMISale {
		return false, intents.ValidateNMISaleTerminal(row)
	}
	if typ == subscriptions.TypeSubscriptionCollection {
		return true, intents.ValidateSubscriptionCollectionTerminal(row)
	}
	if typ == subscriptions.TypeManualRebill {
		if err := intents.ValidateManualRebillTerminal(row); err != nil {
			return false, fmt.Errorf("invalid accepted rebill archive: %w", err)
		}
		terms, _ := subscriptions.DecodeManualRebillPayload(row)
		return terms.Initiator == charge.InitiatorCustomer, nil
	}
	terms, err := intents.DecodeOrderCollectionPayload(row)
	if err != nil {
		return false, fmt.Errorf("invalid accepted collection payload: %w", err)
	}
	_, found, err := intents.LoadCollectedReceipt(row)
	if err != nil {
		return false, fmt.Errorf("invalid qualified collection receipt: %w", err)
	}
	if (row.Status == intents.StatusSucceeded) != found {
		return false, fmt.Errorf("collection terminal state and qualified receipt disagree")
	}
	return terms.Initiator == charge.InitiatorCustomer || row.Origin == string(intents.OriginAdmin) && intents.OrderCollectionRetryKeyValid(terms.OrderID, row.IdempotencyKey), nil
}

func value(p Profile, values []*string, name string) *string {
	for i, c := range p.Columns {
		if c.Name == name {
			return values[i]
		}
	}
	return nil
}
