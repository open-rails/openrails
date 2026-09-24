package contract

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"strconv"
	"strings"
	"time"
)

func ValidateValues(p Profile, values []*string) error {
	if len(values) != len(p.Columns) {
		return fmt.Errorf("invalid row width for %s", p.Name)
	}
	if p.Name == "catalog_applications" {
		var receipt struct {
			ApplicationID   string `json:"application_id"`
			CatalogID       string `json:"catalog_id"`
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
		catalogID := value(p, values, "catalog_id")
		parsed, err := openrails.ParseCatalogID(receipt.CatalogID)
		if err != nil || catalogID == nil || parsed.UUID().String() != *catalogID {
			return fmt.Errorf("catalog application receipt target mismatch")
		}

	}
	if p.Name == "subscriptions" {
		policy, rail, binding := value(p, values, "collection_policy"), value(p, values, "rail"), value(p, values, "rail_subscription_id")
		if policy == nil || !models.CollectionPolicy(*policy).Valid() || rail == nil || binding == nil {
			return fmt.Errorf("subscription lacks a valid collection policy or binding")
		}
		if *policy == "provider_dunning" && *rail != "nmi" {
			return fmt.Errorf("provider dunning requires NMI")
		}
		if *policy == "engine" && !((*rail == "nmi" || *rail == "stripe") && *binding == "" || *rail == "solana") {
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
	if p.Name == "invoice_payments" || p.Name == "ledger_transfers" {
		keyField := "idempotency_key"
		if p.Name == "ledger_transfers" {
			keyField = "source_id"
		}
		key, payer, invoice := value(p, values, keyField), value(p, values, "customer_id"), value(p, values, "invoice_id")
		if key != nil && strings.HasPrefix(*key, "invoice_collection:") {
			payerID, invoiceID := uuid.Nil, uuid.Nil
			if payer != nil {
				payerID, _ = uuid.Parse(*payer)
			}
			if invoice != nil {
				invoiceID, _ = uuid.Parse(*invoice)
			}
			encodedKey = payerID != uuid.Nil && invoiceID != uuid.Nil && (charge.CustomerPaymentKeyValid("invoice_collection", payerID, *key) || intents.InvoiceCollectionRetryKeyValid(invoiceID, *key))
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
		// An owner subject is an opaque host identity, not free-form metadata.
		// Preserve exact UTF-8 bytes (including URL/non-UUID subject formats).
		if p.Name == "catalogs" && c.Name == "owner_subject" {
			if err := catalogscope.ValidateSubject(v); err != nil {
				return err
			}
			continue
		}
		if c.Type == "text" || strings.HasPrefix(c.Type, "character varying") {
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
				if typ := value(p, values, "intent_type"); typ != nil && (*typ == intents.TypeNMIPaymentMethodDelete || *typ == intents.TypeHyperSwitchMethodDelete || *typ == "nmi_sale" || *typ == "initial_membership" || *typ == "invoice_collection" || *typ == "manual_rebill" || *typ == subscriptions.TypeSubscriptionCollection || *typ == "nmi_provider_cutover" || *typ == intents.TypeNMIEngineTakeover) {
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
	if p.Name == "checkout_sessions" {
		field := func(name string) string {
			if v := value(p, values, name); v != nil {
				return *v
			}
			return ""
		}
		var state struct {
			Purchase        json.RawMessage `json:"accepted_purchase"`
			Submitted       bool            `json:"purchase_submitted"`
			Closed          bool            `json:"provider_closed"`
			Capture         json.RawMessage `json:"capture"`
			Kind            string          `json:"kind"`
			CustomerRef     string          `json:"customer_ref"`
			Consent         string          `json:"consent"`
			PaymentMethodID string          `json:"payment_method_id"`
			Quote           string          `json:"initial_membership_quote"`
		}
		if raw := value(p, values, "rail_state"); raw != nil && json.Unmarshal([]byte(*raw), &state) != nil {
			return fmt.Errorf("invalid checkout state")
		}
		if len(state.Purchase) > 0 {
			var terms struct {
				PriceID          uuid.UUID `json:"price_id"`
				ProductID        uuid.UUID `json:"product_id"`
				PaymentID        uuid.UUID `json:"payment_id"`
				Amount           int64     `json:"amount,string"`
				Currency         string
				AcceptedAt       time.Time `json:"accepted_at"`
				EntitlementStart time.Time `json:"entitlement_start"`
				Duration         *int      `json:"access_duration_hours"`
			}

			if json.Unmarshal(state.Purchase, &terms) != nil || terms.PriceID.String() != field("price_id") || terms.ProductID == uuid.Nil || terms.PaymentID == uuid.Nil || terms.Amount < 0 || strconv.FormatInt(terms.Amount, 10) != field("amount") || terms.Currency != field("currency") || field("mode") != "one_off" || terms.AcceptedAt.IsZero() || terms.EntitlementStart.Before(terms.AcceptedAt) || terms.Duration != nil && *terms.Duration <= 0 {
				return fmt.Errorf("invalid retained purchase checkout terms")
			}
			if field("rail") == "stripe" && state.Submitted && !state.Closed && field("status") != "succeeded" {
				return fmt.Errorf("submitted Stripe checkout outcome is unresolved")
			}
		}
		if state.Quote != "" {
			var quote subscriptions.InitialMembershipTerms
			if json.Unmarshal([]byte(state.Quote), &quote) != nil || quote.Validate() != nil || quote.CollectionPolicy != models.CollectionPolicyEngine || quote.CustomerID.String() != field("customer_id") || quote.PSPID.String() != field("psp_id") || quote.PriceID.String() != field("price_id") || strconv.FormatInt(quote.Amount, 10) != field("amount") || quote.Currency != field("currency") || field("mode") != "subscription" || (field("rail") != "nmi" && field("rail") != "stripe") {
				return fmt.Errorf("invalid retained engine checkout quote")
			}
		}
		if field("mode") == string(models.CheckoutSessionModePaymentMethod) && field("rail") == "stripe" {
			for _, name := range []string{"price_id", "amount", "currency", "payment_id", "subscription_id", "transaction_id"} {
				if value(p, values, name) != nil {
					return fmt.Errorf("Stripe setup contains monetary terms")
				}
			}
			if len(state.Capture) != 0 || state.Quote != "" || state.Kind != "stripe_engine_setup" || state.Consent != "save_for_agreed_off_session_payments_v1" || !strings.HasPrefix(state.CustomerRef, "cus_") {
				return fmt.Errorf("invalid Stripe setup binding")
			}
			if field("reference") != "" && !strings.HasPrefix(field("reference"), "seti_") {
				return fmt.Errorf("invalid Stripe setup identity")
			}
			if field("status") == "succeeded" && (field("reference") == "" || !uuidPattern.MatchString(state.PaymentMethodID)) {
				return fmt.Errorf("completed Stripe setup lacks retained method")
			}
		} else if field("mode") == string(models.CheckoutSessionModePaymentMethod) {
			for _, name := range []string{"price_id", "amount", "currency", "payment_id", "subscription_id", "reference", "transaction_id"} {
				if value(p, values, name) != nil {
					return fmt.Errorf("capture setup contains monetary/provider payment terms")
				}
			}
			if field("rail") != "nmi" {
				return fmt.Errorf("unsupported capture rail")
			}
			expiry, err := time.Parse("2006-01-02 15:04:05.999999-07", field("expires_at"))
			if err != nil {
				return fmt.Errorf("invalid capture expiry")
			}
			owner, _ := uuid.Parse(field("merchant_id"))
			customer, _ := uuid.Parse(field("customer_id"))
			psp, _ := uuid.Parse(field("psp_id"))
			if _, err = models.DecodeCheckoutCapture(state.Capture, owner, customer, psp, models.CheckoutSessionStatus(field("status")), &expiry); err != nil {
				return fmt.Errorf("invalid terminal capture binding: %w", err)
			}
		} else if len(state.Capture) != 0 {
			return fmt.Errorf("capture binding on priced checkout")
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
		if payload != nil && *payload != "null" && *payload != "{}" && (typ == nil || (*typ != intents.TypeNMIPaymentMethodDelete && *typ != intents.TypeHyperSwitchMethodDelete && *typ != "nmi_refund" && *typ != "stripe_refund" && *typ != "ccbill_refund" && *typ != "invoice_collection" && *typ != "nmi_sale" && *typ != "initial_membership" && *typ != "manual_rebill" && *typ != subscriptions.TypeSubscriptionCollection && *typ != "nmi_provider_cutover" && *typ != intents.TypeNMIEngineTakeover)) {
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

	}
	return nil
}

// validateRetainedPayment also identifies an engine-generated key. A caller key or arbitrary metadata
// never reaches this exception merely by resembling a hash.
func validateRetainedPayment(p Profile, values []*string) (bool, error) {
	if p.Name != "rail_intents" {
		return false, nil
	}
	field := func(name string) string {
		if v := value(p, values, name); v != nil {
			return *v
		}
		return ""
	}
	typ := field("intent_type")
	if typ != intents.TypeNMIPaymentMethodDelete && typ != intents.TypeHyperSwitchMethodDelete && typ != "invoice_collection" && typ != subscriptions.TypeManualRebill && typ != subscriptions.TypeSubscriptionCollection && typ != payments.TypeNMISale && typ != subscriptions.TypeInitialMembership {
		return false, nil
	}
	id, _ := uuid.Parse(field("id"))
	merchant, _ := uuid.Parse(field("merchant_id"))
	row := gen.OpenrailsRailIntent{ID: id, MerchantID: merchant, Rail: field("rail"), IntentType: typ, Payload: []byte(field("payload")), ResultEvidence: []byte(field("result_evidence")), Status: field("status"), Origin: field("origin"), IdempotencyKey: field("idempotency_key")}
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
	terms, err := intents.DecodeInvoiceCollectionPayload(row)
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
	return terms.Initiator == charge.InitiatorCustomer || row.Origin == string(intents.OriginAdmin) && intents.InvoiceCollectionRetryKeyValid(terms.InvoiceID, row.IdempotencyKey), nil
}

func value(p Profile, values []*string, name string) *string {
	for i, c := range p.Columns {
		if c.Name == name {
			return values[i]
		}
	}
	return nil
}
