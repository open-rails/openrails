package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TypeNMISubscriptionCreate is the checkout NMI recurring create (#674): the
// atomic first-charge + enroll (classic recurring=add_subscription) posted as
// a durable write-ahead intent and executed inline. The NMI order id is
// derived from the intent id; a timeout after NMI created the subscription no
// longer orphans a live remote subscription — the verify leg re-finds it (by
// order-id sale search + a vault+plan roster scan) and completes the local
// registration.
const TypeNMISubscriptionCreate = "nmi_subscription_create"

// NMISubscriptionCreateIdempotencyKey addresses one logical checkout
// subscription create by the client's checkout idempotency key.
func NMISubscriptionCreateIdempotencyKey(checkoutIdempotencyKey string) string {
	return TypeNMISubscriptionCreate + ":" + strings.TrimSpace(checkoutIdempotencyKey)
}

// NMISubscriptionCreatePayload carries everything Execute and the async
// verifier need to create the remote subscription AND register it locally
// without the originating HTTP request.
type NMISubscriptionCreatePayload struct {
	// Provider is the RAIL ("nmi") — local row vocabulary.
	Provider string `json:"provider"`
	// PSP is the payment provider (account key, e.g. "mobius")
	// this create charges through; "" resolves the rail's active account.
	PSP             string `json:"psp,omitempty"`
	PlanID          string `json:"plan_id"`
	CustomerVaultID string `json:"customer_vault_id"`
	// BillingID binds the subscription to ONE stored card in the vault (#682
	// shared-vault support); "" uses the vault's priority-1 entry.
	BillingID           string     `json:"billing_id,omitempty"`
	AmountMicros        int64      `json:"amount_micros"`
	Currency            string     `json:"currency"`
	Email               string     `json:"email,omitempty"`
	UserID              string     `json:"user_id"`
	PriceID             uuid.UUID  `json:"price_id"`
	LocalSubscriptionID uuid.UUID  `json:"local_subscription_id"`
	PaymentMethodID     *uuid.UUID `json:"payment_method_id,omitempty"`
	// StartDate (YYYYMMDD, "" = immediate) + DelayedStart mirror
	// nmiSubscriptionStartDate's coverage-derived delayed start.
	StartDate    string     `json:"start_date,omitempty"`
	DelayedStart *time.Time `json:"delayed_start,omitempty"`
	// StoredCredentialRef is the instrument's RECURRING-sequence
	// stored-credential anchor at enqueue (#297). "" = this enrollment is the
	// sequence's initial CIT (indicator=stored) and finalize captures the
	// first-charge transaction id as the anchor (delayed starts produce no
	// first charge — the anchor then back-fills from the first dunning MIT).
	StoredCredentialRef string `json:"stored_credential_ref,omitempty"`
	E2ERunID            string `json:"e2e_run_id,omitempty"`
	// CheckoutIdempotencyKey lets finalize complete the request-level
	// idempotency record so a client replay gets the cached response.
	CheckoutIdempotencyKey string `json:"checkout_idempotency_key"`

	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Address1  string `json:"address1,omitempty"`
	City      string `json:"city,omitempty"`
	State     string `json:"state,omitempty"`
	Zip       string `json:"zip,omitempty"`
	Country   string `json:"country,omitempty"`
}

// Evidence keys the producer reads back off a succeeded intent.
const (
	nmiSubEvidenceSubscriptionID = "subscription_id"
	nmiSubEvidenceStatus         = "status"
	nmiSubEvidenceMessage        = "message"
)

// NMISubscriptionCreateIntentHandler executes checkout recurring creates with
// money-mover semantics: attempts > 1 verify-at-provider first; transport
// ambiguity parks as unknown_needs_verify; a create that landed at NMI but
// failed local registration keeps resolving through the verifier until the
// subscription is registered — never a live remote subscription with no local
// row.
type NMISubscriptionCreateIntentHandler struct {
	Checkout *CheckoutService
	Policy   intents.BackoffPolicy
}

func NewNMISubscriptionCreateIntentHandler(checkoutService *CheckoutService) *NMISubscriptionCreateIntentHandler {
	return &NMISubscriptionCreateIntentHandler{Checkout: checkoutService, Policy: intents.DefaultBackoff}
}

func (h *NMISubscriptionCreateIntentHandler) Type() string { return TypeNMISubscriptionCreate }
func (h *NMISubscriptionCreateIntentHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

// PrunePolicy keeps result_evidence: the producer reads
// subscription_id/transaction_id/delayed_start off the durable row.
func (h *NMISubscriptionCreateIntentHandler) PrunePolicy() (keepPayload, keepEvidence bool) {
	return false, true
}

func decodeNMISubscriptionCreatePayload(intent gen.OpenrailsRailIntent) (NMISubscriptionCreatePayload, error) {
	var p NMISubscriptionCreatePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("nmi subscription create intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode nmi subscription create payload: %w", err)
	}
	if p.PlanID == "" || p.CustomerVaultID == "" || p.UserID == "" ||
		p.PriceID == uuid.Nil || p.LocalSubscriptionID == uuid.Nil {
		return p, errors.New("nmi subscription create payload is incomplete")
	}
	return p, nil
}

// CheckRelevance: registration (finalize) is idempotent and a decline is
// terminal, so the intent never goes stale on its own.
func (h *NMISubscriptionCreateIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}

func (h *NMISubscriptionCreateIntentHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	if h.Checkout == nil {
		return intents.Parked("checkout service not wired")
	}
	p, err := decodeNMISubscriptionCreatePayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	client, err := h.Checkout.resolveNMIClient(ctx, nmiIntentClientName(p.PSP, intent.Rail))
	if err != nil {
		return intents.Parked(fmt.Sprintf("nmi client not configured for provider %q: %v", nmiIntentClientName(p.PSP, intent.Rail), err))
	}
	if client.ReadOnly {
		return intents.Parked("nmi client is read-only (mode=readonly)")
	}
	orderID := nmiSaleIntentOrderID(intent.ID, p.E2ERunID)

	// Money mover + remote-resource creator: re-executions verify first.
	if intent.Attempts > 1 {
		if outcome, resolved := h.verifyAtProvider(ctx, intent.MerchantID, client, p, orderID); resolved {
			return outcome
		}
	}

	// NMI charges whole cents; the payload carries micros. Error (never round)
	// on a sub-cent remainder — same policy as the one-time sale path. Terminal,
	// not parked: no retry can make an unrepresentable price representable, and
	// nothing was sent, so the checkout fails clean instead of under-charging.
	amountCents, err := moneyutil.MicrosToCentsExact(moneyutil.Micros(p.AmountMicros))
	if err != nil {
		return intents.Terminal("subscription amount must be representable in whole cents: " + err.Error())
	}

	// #297: subscription enrollment is the cardholder-initiated RECURRING
	// credential-on-file charge — the sequence's initial CIT when the
	// instrument has no recurring anchor, a reuse when it does (a second
	// subscription on an already-anchored card).
	citContext := charge.InitialRecurring()
	if p.StoredCredentialRef != "" {
		citContext = charge.RecurringReuse(p.StoredCredentialRef)
	}
	resp, err := client.AddRecurringSubscription(nmi.RecurringPaymentData{
		BillingID: p.BillingID,
		CardUserData: nmi.CardUserData{
			FirstName: p.FirstName,
			LastName:  p.LastName,
			Address1:  p.Address1,
			City:      p.City,
			State:     p.State,
			Zip:       p.Zip,
			Country:   p.Country,
		},
		PlanID:           p.PlanID,
		CustomerVaultID:  p.CustomerVaultID,
		Amount:           moneyutil.Cents(amountCents),
		Currency:         p.Currency,
		Email:            p.Email,
		OrderID:          orderID,
		PONumber:         orderID,
		CustomerID:       p.UserID,
		StartDate:        p.StartDate,
		StoredCredential: nmidirect.StoredCredentialFor(citContext),
	})
	if err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return intents.Parked("nmi provider writes blocked (mode=readonly)")
		}
		if nmi.IsTransportAmbiguous(err) {
			// The subscription may exist at NMI; the verifier re-finds it.
			return intents.Ambiguous("subscription create outcome unknown: " + err.Error())
		}
		var vaultErr *nmi.CustomerVaultError
		if errors.As(err, &vaultErr) {
			// #796: record the declined enrollment charge attempt (verbatim code).
			if h.Checkout != nil && h.Checkout.PurchaseService != nil {
				recordDeclinedAttempt(ctx, h.Checkout.PurchaseService.PaymentService, DeclinedAttempt{
					UserID:                 p.UserID,
					PriceID:                p.PriceID,
					Rail:                   strings.ToLower(p.Provider),
					SyntheticTransactionID: "nmi_sub_declined:" + intent.ID.String(),
					AmountMicros:           p.AmountMicros,
					Currency:               p.Currency,
					FailureCode:            nmidirect.FailureCode(vaultErr),
					AttemptKind:            payments.AttemptInitial,
					TokenType:              charge.TokenTypeProviderVault,
				})
			}
			return intents.TerminalWithEvidence(vaultErr.Error(), map[string]any{
				"declined":        true,
				"response_code":   vaultErr.ResponseCode,
				"localization_id": vaultErr.LocalizationID,
			})
		}
		return intents.Terminal("subscription create request rejected: " + err.Error())
	}
	return h.finalize(ctx, intent.MerchantID, p, orderID, resp.SubscriptionID, resp.TransactionID, false)
}

// Verify resolves an ambiguous create via provider READS.
func (h *NMISubscriptionCreateIntentHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	p, err := decodeNMISubscriptionCreatePayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	client, err := h.Checkout.resolveNMIClient(ctx, nmiIntentClientName(p.PSP, intent.Rail))
	if err != nil {
		return intents.Ambiguous(fmt.Sprintf("nmi client not configured for provider %q; cannot verify", nmiIntentClientName(p.PSP, intent.Rail)))
	}
	orderID := nmiSaleIntentOrderID(intent.ID, p.E2ERunID)
	if outcome, resolved := h.verifyAtProvider(ctx, intent.MerchantID, client, p, orderID); resolved {
		return outcome
	}
	return intents.Retryable("no remote subscription or sale found for this intent; create verified not executed")
}

// verifyAtProvider answers "did THIS create land at NMI?" via reads:
//   - the order-id sale search finds the atomic first charge (immediate starts);
//   - a subscription-roster scan finds the enrolled record by (vault, plan) —
//     the delayed-start case, which produces no first charge. A remote match is
//     accepted when it is unknown locally (the orphan this issue exists to
//     catch) or when its local row carries THIS intent's order id (finalize
//     crashed midway; re-finalize).
//
// resolved=false means "verified not executed" — the caller may (re)send.
func (h *NMISubscriptionCreateIntentHandler) verifyAtProvider(ctx context.Context, merchantID uuid.UUID, client *nmi.NMIClient, p NMISubscriptionCreatePayload, orderID string) (intents.Outcome, bool) {
	txnID, txnFound, err := client.FindSuccessfulSaleByOrderID(orderID)
	if err != nil {
		return intents.Ambiguous("pre-send verification read failed: " + err.Error()), true
	}

	candidates, err := findUnregisteredRemoteSubscriptions(ctx, h.Checkout.SubscriptionService, client, strings.ToLower(p.Provider), p.CustomerVaultID, p.PlanID, orderID)
	if err != nil {
		return intents.Ambiguous(err.Error()), true
	}

	switch {
	case len(candidates) == 1:
		return h.finalize(ctx, merchantID, p, orderID, candidates[0], txnID, true), true
	case len(candidates) > 1:
		return intents.Ambiguous(fmt.Sprintf("%d unregistered remote subscriptions match vault %s plan %s; operator attention required",
			len(candidates), p.CustomerVaultID, p.PlanID)), true
	case txnFound:
		// Charged but no matching enrollment found: never resend (that would
		// double-charge); keep verifying.
		return intents.Ambiguous("successful sale found for order id but no matching remote subscription; keeping under verification"), true
	default:
		return intents.Outcome{}, false
	}
}

// railSubscriptionReader is the local-lookup surface the roster scan needs
// (satisfied by *subscriptions.SubscriptionService).
type railSubscriptionReader interface {
	GetByRailSubscriptionID(ctx context.Context, provider, railSubscriptionID string) (*models.Subscription, error)
}

// findUnregisteredRemoteSubscriptions scans the NMI recurring roster for
// subscriptions on (vault, plan) that are unknown locally, or whose local row
// carries the given order id (a partially-completed finalize). Shared by the
// nmi_subscription_create verify leg and the upgrade successor-create
// ambiguity recovery (#674): both answer "did OUR create land at NMI?".
func findUnregisteredRemoteSubscriptions(ctx context.Context, subs railSubscriptionReader, client *nmi.NMIClient, provider, vaultID, planID, orderID string) ([]string, error) {
	var candidates []string
	cursor := ""
	for {
		page, perr := client.ListSubscriptionsPage(cursor, 0)
		if perr != nil {
			return nil, fmt.Errorf("subscription roster read failed: %w", perr)
		}
		for _, sub := range page.Subscriptions {
			if strings.TrimSpace(sub.CustomerVaultID) != strings.TrimSpace(vaultID) {
				continue
			}
			if sub.Plan == nil || strings.TrimSpace(sub.Plan.ID) != strings.TrimSpace(planID) {
				continue
			}
			local, lerr := subs.GetByRailSubscriptionID(ctx, provider, sub.ID)
			if lerr != nil {
				if !db.IsNotFound(lerr) {
					return nil, fmt.Errorf("local subscription lookup failed: %w", lerr)
				}
				// Unknown locally: the orphan the create left behind.
				candidates = append(candidates, sub.ID)
				continue
			}
			if orderID != "" && subscriptionMetadataString(local.Metadata, "order_id") == orderID {
				// Registered by a partially-completed finalize of THIS intent.
				candidates = append(candidates, sub.ID)
			}
		}
		next := string(page.NextCursor)
		if !page.HasMore || next == "" {
			break
		}
		cursor = next
	}
	return candidates, nil
}

// nmiIntentClientName picks the client-resolution name for an NMI intent: the
// payload's pinned PSP, else the intent's rail (active account).
func nmiIntentClientName(psp, rail string) string {
	if name := strings.ToLower(strings.TrimSpace(psp)); name != "" {
		return name
	}
	return strings.ToLower(strings.TrimSpace(rail))
}

// subscriptionMetadataString reads one string key off a subscription's raw
// JSON metadata.
func subscriptionMetadataString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// finalize registers the confirmed remote subscription locally through the
// standard registration path (idempotent: existing rows are activated /
// answered, not duplicated) and completes the request-level idempotency
// record so client replays get the cached response.
func (h *NMISubscriptionCreateIntentHandler) finalize(ctx context.Context, merchantID uuid.UUID, p NMISubscriptionCreatePayload, orderID, providerSubscriptionID, transactionID string, verified bool) intents.Outcome {
	if strings.TrimSpace(providerSubscriptionID) == "" {
		return intents.Ambiguous("remote subscription id unavailable; cannot register locally")
	}

	// #297: an initial recurring CIT anchors the instrument's recurring
	// sequence with its first-charge transaction id. Delayed starts have no
	// first charge (transactionID "") — the anchor then back-fills from the
	// first successful dunning MIT instead. Best-effort, write-once.
	if p.StoredCredentialRef == "" && strings.TrimSpace(transactionID) != "" {
		h.captureStoredCredentialRef(ctx, merchantID, p, strings.TrimSpace(transactionID))
	}
	price, err := h.Checkout.PriceService.GetByID(ctx, p.PriceID)
	if err != nil {
		return intents.Ambiguous("load price for registration: " + err.Error())
	}
	product, err := h.Checkout.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return intents.Ambiguous("load product for registration: " + err.Error())
	}
	req := &CheckoutRequest{Email: p.Email}
	if p.E2ERunID != "" {
		req.Metadata = map[string]string{"e2e_run_id": p.E2ERunID}
	}
	user := &UserIdentity{ID: p.UserID}

	resp, err := h.Checkout.completeNMISubscriptionRegistration(
		ctx, req, user, price, product, strings.ToLower(p.Provider),
		p.LocalSubscriptionID, providerSubscriptionID, transactionID,
		p.DelayedStart, orderID, p.PaymentMethodID,
		"nmi_subscription", p.CheckoutIdempotencyKey,
	)
	if err != nil {
		// The remote subscription EXISTS; keep resolving through the verifier
		// until registration lands — never a live remote sub with no local row.
		return intents.Ambiguous("subscription created at provider, but local registration failed: " + err.Error())
	}

	evidence := map[string]any{
		nmiSaleEvidenceTransactionID: transactionID,
		nmiSubEvidenceStatus:         resp.Status,
		nmiSubEvidenceMessage:        resp.Message,
	}
	if resp.SubscriptionID != nil {
		evidence[nmiSubEvidenceSubscriptionID] = resp.SubscriptionID.String()
	}
	if resp.DelayedStart != nil {
		evidence[nmiSaleEvidenceDelayedStart] = resp.DelayedStart.UTC().Format(time.RFC3339)
	}
	if verified {
		evidence["verified_existing"] = true
	}
	return intents.Succeeded(evidence)
}

// captureStoredCredentialRef persists the recurring-sequence anchor for the
// enrolled instrument (#297), keyed by the rail handles the payload carries.
// Best-effort: a miss means the next successful recurring charge re-captures.
func (h *NMISubscriptionCreateIntentHandler) captureStoredCredentialRef(ctx context.Context, merchantID uuid.UUID, p NMISubscriptionCreatePayload, transactionID string) {
	if h.Checkout == nil || h.Checkout.VaultService == nil || h.Checkout.VaultService.DB == nil {
		log.WithContext(ctx).Warn("nmi subscription finalize: no DB handle to persist stored-credential reference (#297)")
		return
	}
	if _, err := h.Checkout.VaultService.DB.Gen(ctx).CaptureStoredCredentialRefByRailInstrument(ctx, gen.CaptureStoredCredentialRefByRailInstrumentParams{
		MerchantID:      merchantID,
		Rail:            strings.ToLower(strings.TrimSpace(p.Provider)),
		RailCustomerRef: strings.TrimSpace(p.CustomerVaultID),
		RailMethodRef:   strings.TrimSpace(p.BillingID),
		Agreement:       string(charge.AgreementRecurring),
		Ref:             transactionID,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithField("customer_vault_id", p.CustomerVaultID).
			Warn("nmi subscription finalize: failed to persist stored-credential reference (#297); next recurring charge re-captures")
	}
}
