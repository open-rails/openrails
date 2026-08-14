package intents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TypeNMIPaymentMethodUpdate is the durable stored-card replacement intent.
// Collect.js tokens are single-use, so an ambiguous submission is resolved by
// reading the exact NMI billing entry; the token is never blindly resubmitted.
const TypeNMIPaymentMethodUpdate = "nmi_payment_method_update"

const collectJSTokenLifetime = 24 * time.Hour

func NMIPaymentMethodUpdateIdempotencyKey(paymentMethodID uuid.UUID, paymentToken string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(paymentToken)))
	return fmt.Sprintf("%s:%s:%s", TypeNMIPaymentMethodUpdate, paymentMethodID, hex.EncodeToString(digest[:16]))
}

type NMIPaymentMethodUpdatePayload struct {
	UserID          string    `json:"user_id"`
	PaymentMethodID uuid.UUID `json:"payment_method_id"`
	RailCustomerRef string    `json:"rail_customer_ref"`
	RailMethodRef   string    `json:"rail_method_ref,omitempty"`
	PaymentToken    string    `json:"payment_token"`
	FirstName       string    `json:"first_name,omitempty"`
	LastName        string    `json:"last_name,omitempty"`
	Address1        string    `json:"address1,omitempty"`
	City            string    `json:"city,omitempty"`
	State           string    `json:"state,omitempty"`
	Zip             string    `json:"zip,omitempty"`
	Country         string    `json:"country,omitempty"`
	Phone           string    `json:"phone,omitempty"`
	Email           string    `json:"email,omitempty"`
	Company         string    `json:"company,omitempty"`
	Address2        string    `json:"address2,omitempty"`
	TargetCard      nmiCard   `json:"target_card"`
}

type nmiCard struct {
	LastFour   string `json:"last_four"`
	CardType   string `json:"card_type"`
	ExpiryDate string `json:"expiry_date"`
}

func (c nmiCard) complete() bool {
	return c.LastFour != "" && c.CardType != "" && c.ExpiryDate != ""
}

func (c nmiCard) matches(other nmiCard) bool {
	return c.LastFour == other.LastFour &&
		canonicalCardType(c.CardType) == canonicalCardType(other.CardType) &&
		c.ExpiryDate == other.ExpiryDate
}

type nmiPaymentMethodUpdateProgress struct {
	SubmissionStarted bool    `json:"submission_started"`
	OldCard           nmiCard `json:"old_card"`
}

func decodeNMIPaymentMethodUpdatePayload(intent gen.OpenrailsRailIntent) (NMIPaymentMethodUpdatePayload, error) {
	var payload NMIPaymentMethodUpdatePayload
	if len(intent.Payload) == 0 {
		return payload, errors.New("nmi payment method update intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &payload); err != nil {
		return payload, fmt.Errorf("decode nmi payment method update payload: %w", err)
	}
	if payload.PaymentMethodID == uuid.Nil || strings.TrimSpace(payload.RailCustomerRef) == "" ||
		strings.TrimSpace(payload.PaymentToken) == "" || !payload.TargetCard.complete() {
		return payload, errors.New("nmi payment method update payload is incomplete")
	}
	return payload, nil
}

func decodeNMIPaymentMethodUpdateProgress(intent gen.OpenrailsRailIntent) (nmiPaymentMethodUpdateProgress, error) {
	var progress nmiPaymentMethodUpdateProgress
	if len(intent.ResultEvidence) == 0 {
		return progress, nil
	}
	if err := json.Unmarshal(intent.ResultEvidence, &progress); err != nil {
		return progress, fmt.Errorf("decode nmi payment method update progress: %w", err)
	}
	return progress, nil
}

type NMIPaymentMethodUpdateHandler struct {
	DB            *db.DB
	Rails         RailClientResolver
	Store         *Store
	Clock         clockwork.Clock
	Policy        BackoffPolicy
	finalizeWrite func(context.Context, *models.PaymentMethod) error
}

func NewNMIPaymentMethodUpdateHandler(d *db.DB, rails RailClientResolver, store *Store, clock clockwork.Clock) *NMIPaymentMethodUpdateHandler {
	return &NMIPaymentMethodUpdateHandler{DB: d, Rails: rails, Store: store, Clock: clock, Policy: DefaultBackoff}
}

func (h *NMIPaymentMethodUpdateHandler) Type() string { return TypeNMIPaymentMethodUpdate }
func (h *NMIPaymentMethodUpdateHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}
func (h *NMIPaymentMethodUpdateHandler) PruneTerminalPayload() bool { return true }

func (h *NMIPaymentMethodUpdateHandler) now() time.Time {
	if h.Clock != nil {
		return h.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

// Relevance is resolved from the exact provider card inside Execute/Verify;
// there is no separate local desired-state field that can supersede the intent.
func (h *NMIPaymentMethodUpdateHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (Relevance, error) {
	return StillRelevant(), nil
}

func (h *NMIPaymentMethodUpdateHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	payload, err := decodeNMIPaymentMethodUpdatePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	pm, client, outcome, ok := h.dependencies(ctx, intent, payload)
	if !ok {
		return outcome
	}
	progress, err := decodeNMIPaymentMethodUpdateProgress(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	remote, present, err := readNMIPaymentMethodCard(ctx, client, payload.RailCustomerRef, payload.RailMethodRef)
	if err != nil {
		return Retryable("provider read before card replacement failed: " + err.Error())
	}
	if !present {
		return Terminal("stored card is absent at NMI; replacement cannot apply")
	}

	if progress.SubmissionStarted {
		return h.reconcile(ctx, intent, payload, pm, remote, progress.OldCard)
	}
	if !intent.CreatedAt.IsZero() && !h.now().Before(intent.CreatedAt.Add(collectJSTokenLifetime)) {
		return retokenizeTerminal("Collect.js token expired before the replacement could be submitted")
	}
	if h.Store == nil {
		return Parked("payment method update progress store not wired")
	}
	if err := h.Store.RecordProgress(ctx, intent.ID, map[string]any{
		"submission_started": true,
		"old_card":           remote,
	}); err != nil {
		return Retryable("record card replacement submission boundary: " + err.Error())
	}

	if err := client.UpdateCustomerVault(ctx, payload.providerUpdate()); err != nil {
		switch {
		case errors.Is(err, nmi.ErrProviderReadOnly):
			return Parked("nmi provider writes blocked (mode=readonly)")
		case nmi.IsTransportAmbiguous(err):
			return Ambiguous("card replacement outcome unknown: " + err.Error())
		default:
			return Terminal("card replacement rejected cleanly: " + err.Error())
		}
	}

	confirmed, present, err := readNMIPaymentMethodCard(ctx, client, payload.RailCustomerRef, payload.RailMethodRef)
	if err != nil {
		return Ambiguous("card replacement accepted, but confirmation read failed: " + err.Error())
	}
	if !present {
		return Ambiguous("card replacement accepted, but the stored card disappeared before confirmation")
	}
	if !payload.TargetCard.matches(confirmed) {
		return Ambiguous("card replacement accepted, but NMI has not confirmed the requested masked card")
	}
	return h.finalize(ctx, intent, pm, confirmed, "provider_confirmed_inline")
}

func (h *NMIPaymentMethodUpdateHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	payload, err := decodeNMIPaymentMethodUpdatePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	pm, client, outcome, ok := h.dependencies(ctx, intent, payload)
	if !ok {
		return outcome
	}
	progress, err := decodeNMIPaymentMethodUpdateProgress(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	if !progress.SubmissionStarted || !progress.OldCard.complete() {
		return Terminal("card replacement is missing its durable submission boundary")
	}
	remote, present, err := readNMIPaymentMethodCard(ctx, client, payload.RailCustomerRef, payload.RailMethodRef)
	if err != nil {
		return Ambiguous("provider read while verifying card replacement failed: " + err.Error())
	}
	if !present {
		return Terminal("stored card disappeared at NMI while replacement was unresolved")
	}
	return h.reconcile(ctx, intent, payload, pm, remote, progress.OldCard)
}

func (h *NMIPaymentMethodUpdateHandler) dependencies(ctx context.Context, intent gen.OpenrailsRailIntent, payload NMIPaymentMethodUpdatePayload) (*models.PaymentMethod, *nmi.NMIClient, Outcome, bool) {
	pm, err := paymentmethods.NewPaymentMethodRepo(h.DB).GetByID(ctx, payload.PaymentMethodID)
	if err != nil {
		if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
			return nil, nil, Terminal("payment method row no longer exists"), false
		}
		return nil, nil, Retryable("load payment method: " + err.Error()), false
	}
	if pm.CustomerID.String() != payload.UserID || strings.TrimSpace(pm.RailCustomerRef) != strings.TrimSpace(payload.RailCustomerRef) ||
		strings.TrimSpace(pm.RailMethodRef) != strings.TrimSpace(payload.RailMethodRef) {
		return nil, nil, Terminal("payment method identity changed while card replacement was pending"), false
	}
	if h.Rails == nil {
		return nil, nil, Parked("rail client resolver not wired"), false
	}
	client, err := h.Rails.ResolveClientForPaymentMethod(ctx, pm)
	if err != nil {
		return nil, nil, Parked(fmt.Sprintf("nmi client not configured for provider %q: %v", intent.Rail, err)), false
	}
	if client.ReadOnly {
		return nil, nil, Parked("nmi client is read-only (mode=readonly)"), false
	}
	return pm, client, Outcome{}, true
}

func (h *NMIPaymentMethodUpdateHandler) reconcile(ctx context.Context, intent gen.OpenrailsRailIntent, payload NMIPaymentMethodUpdatePayload, pm *models.PaymentMethod, remote, old nmiCard) Outcome {
	switch {
	case payload.TargetCard.matches(remote):
		return h.finalize(ctx, intent, pm, remote, "provider_confirmed_by_read")
	case old.complete() && old.matches(remote):
		return retokenizeTerminal("NMI still has the original card; the single-use replacement token cannot be submitted again")
	default:
		return TerminalWithEvidence(
			"NMI has a different card than both the original and requested replacement; refusing to overwrite out-of-band state",
			map[string]any{"provider_card": remote},
		)
	}
}

func (h *NMIPaymentMethodUpdateHandler) finalize(ctx context.Context, intent gen.OpenrailsRailIntent, pm *models.PaymentMethod, remote nmiCard, confirmation string) Outcome {
	latest, err := paymentmethods.NewPaymentMethodRepo(h.DB).GetByID(ctx, pm.ID)
	if err != nil {
		if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
			return Terminal("payment method row was removed before card replacement could finalize")
		}
		return Ambiguous("load payment method for local finalize: " + err.Error())
	}
	latest.LastFour = stringPtr(remote.LastFour)
	latest.CardType = stringPtr(remote.CardType)
	latest.ExpiryDate = stringPtr(remote.ExpiryDate)
	latest.UpdatedAt = h.now()
	finalize := h.finalizeWrite
	if finalize == nil {
		finalize = paymentmethods.NewPaymentMethodRepo(h.DB).Update
	}
	if err := finalize(ctx, latest); err != nil {
		return Ambiguous("provider card confirmed, but local finalize failed: " + err.Error())
	}
	return Succeeded(map[string]any{
		"confirmation":      confirmation,
		"payment_method_id": latest.ID,
		"provider_card":     remote,
		"intent_id":         intent.ID,
	})
}

func (p NMIPaymentMethodUpdatePayload) providerUpdate() nmi.UpdateCustomerVaultData {
	return nmi.UpdateCustomerVaultData{
		CustomerVaultID: p.RailCustomerRef,
		BillingID:       p.RailMethodRef,
		CreateCustomerVaultData: nmi.CreateCustomerVaultData{
			PaymentToken: p.PaymentToken,
			FirstName:    p.FirstName,
			LastName:     p.LastName,
			Address1:     p.Address1,
			City:         p.City,
			State:        p.State,
			Zip:          p.Zip,
			Country:      p.Country,
			Phone:        p.Phone,
			Email:        p.Email,
			Company:      p.Company,
			Address2:     p.Address2,
		},
	}
}

func readNMIPaymentMethodCard(ctx context.Context, client *nmi.NMIClient, vaultID, billingID string) (nmiCard, bool, error) {
	page, err := client.ListCustomersPage(ctx, "", 5, vaultID)
	if err != nil {
		return nmiCard{}, false, err
	}
	for i := range page.Customers {
		customer := &page.Customers[i]
		if strings.TrimSpace(customer.ID) != strings.TrimSpace(vaultID) {
			continue
		}
		var billing *nmi.V5CustomerBilling
		if ref := strings.TrimSpace(billingID); ref != "" {
			for j := range customer.Billing {
				if strings.TrimSpace(customer.Billing[j].ID) == ref {
					billing = &customer.Billing[j]
					break
				}
			}
		} else {
			billing = customer.PrimaryBilling()
		}
		if billing == nil {
			return nmiCard{}, false, nil
		}
		card := nmiCard{
			LastFour:   maskedLastFour(billing.PaymentDetails.CardNumber),
			CardType:   strings.TrimSpace(billing.PaymentDetails.CardType),
			ExpiryDate: normalizeNMIExpiry(billing.PaymentDetails.CardExp),
		}
		if !card.complete() {
			return nmiCard{}, false, errors.New("NMI billing entry returned incomplete masked card metadata")
		}
		return card, true, nil
	}
	return nmiCard{}, false, nil
}

func maskedLastFour(value string) string {
	var digits strings.Builder
	for _, r := range value {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	out := digits.String()
	if len(out) < 4 {
		return ""
	}
	return out[len(out)-4:]
}

func normalizeNMIExpiry(value string) string {
	var digits strings.Builder
	for _, r := range value {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	raw := digits.String()
	switch len(raw) {
	case 4:
		return raw[:2] + "/" + raw[2:]
	case 6:
		return raw[:2] + "/" + raw[4:]
	default:
		return ""
	}
}

func canonicalCardType(value string) string {
	var canonical strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			canonical.WriteRune(r)
		}
	}
	return canonical.String()
}

func stringPtr(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func retokenizeTerminal(reason string) Outcome {
	return TerminalWithEvidence(reason, map[string]any{"retokenize": true})
}

type PaymentMethodUpdateThrough struct {
	Runner *Runner
	DB     *db.DB
}

func (t *PaymentMethodUpdateThrough) ExecutePaymentMethodUpdate(ctx context.Context, pm *models.PaymentMethod, req *paymentmethods.UpdatePaymentMethodRequest) (paymentmethods.PaymentMethodUpdateOutcome, error) {
	if t == nil || t.Runner == nil || t.DB == nil {
		return paymentmethods.PaymentMethodUpdateOutcome{}, errors.New("payment method update intent runner not wired")
	}
	if pm == nil || req == nil || req.PaymentToken == nil {
		return paymentmethods.PaymentMethodUpdateOutcome{}, errors.New("payment method and replacement are required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return paymentmethods.PaymentMethodUpdateOutcome{}, err
	}
	payload := NMIPaymentMethodUpdatePayload{
		UserID:          pm.CustomerID.String(),
		PaymentMethodID: pm.ID,
		RailCustomerRef: pm.RailCustomerRef,
		RailMethodRef:   pm.RailMethodRef,
		PaymentToken:    strings.TrimSpace(*req.PaymentToken),
		FirstName:       valueOrEmpty(req.FirstName),
		LastName:        valueOrEmpty(req.LastName),
		Address1:        valueOrEmpty(req.Address1),
		City:            valueOrEmpty(req.City),
		State:           valueOrEmpty(req.State),
		Zip:             valueOrEmpty(req.Zip),
		Country:         valueOrEmpty(req.Country),
		Phone:           valueOrEmpty(req.Phone),
		Email:           valueOrEmpty(req.Email),
		Company:         valueOrEmpty(req.Company),
		Address2:        valueOrEmpty(req.Address2),
		TargetCard: nmiCard{
			LastFour:   valueOrEmpty(req.LastFour),
			CardType:   valueOrEmpty(req.CardType),
			ExpiryDate: valueOrEmpty(req.ExpiryDate),
		},
	}
	row, err := t.Runner.EnqueueAndExecute(ctx, EnqueueParams{
		MerchantID:     tid.UUID(),
		Provider:       strings.ToLower(string(pm.Rail)),
		IntentType:     TypeNMIPaymentMethodUpdate,
		PspID:          pm.PspID,
		Payload:        payload,
		IdempotencyKey: NMIPaymentMethodUpdateIdempotencyKey(pm.ID, payload.PaymentToken),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         OriginUser,
		OriginReason:   "user stored-card replacement",
	})
	if err != nil {
		return paymentmethods.PaymentMethodUpdateOutcome{}, err
	}
	out := paymentmethods.PaymentMethodUpdateOutcome{}
	if row.LastFailureReason != nil {
		out.Reason = *row.LastFailureReason
	}
	switch row.Status {
	case StatusSucceeded:
		out.Done = true
		out.Method, err = paymentmethods.NewPaymentMethodRepo(t.DB).GetByID(ctx, pm.ID)
		if err != nil {
			return paymentmethods.PaymentMethodUpdateOutcome{}, fmt.Errorf("load updated payment method: %w", err)
		}
	case StatusFailedTerminal, StatusSuperseded, StatusExpired:
		out.Terminal = true
		out.Retokenize = intentEvidenceBool(row.ResultEvidence, "retokenize")
	}
	return out, nil
}

func intentEvidenceBool(raw []byte, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var evidence map[string]any
	if json.Unmarshal(raw, &evidence) != nil {
		return false
	}
	value, _ := evidence[key].(bool)
	return value
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
