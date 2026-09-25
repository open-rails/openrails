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
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
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
	NameOnCard      string    `json:"name_on_card,omitempty"`
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

// complete: last four and expiry identify a masked card. NMI may omit the
// brand, so it is optional.
func (c nmiCard) complete() bool {
	return c.LastFour != "" && c.ExpiryDate != ""
}

// matches compares brands only when both sides state one.
func (c nmiCard) matches(other nmiCard) bool {
	a, b := canonicalCardType(c.CardType), canonicalCardType(other.CardType)
	return c.LastFour == other.LastFour && c.ExpiryDate == other.ExpiryDate && (a == "" || b == "" || a == b)
}

// errNMICardDataGap is a stored card NMI reports without its last four or
// expiry: deterministic, so retrying cannot resolve it.
var errNMICardDataGap = errors.New("NMI billing entry returned incomplete masked card metadata")

// PaymentMethodDataGapFinding is raised when a card replacement stops on
// incomplete provider card data.
const PaymentMethodDataGapFinding = "life.payment_method_update.provider_data_gap"

// nmiPaymentMethodUpdateProgress is the replacement's durable boundary. The
// new card is staged as a further billing entry of the same vault and
// verified there; only an approved verification moves the method onto it, so
// the local card and its recurring agreement always describe one card, and a
// refused card leaves the previous card and agreement in use.
type nmiPaymentMethodUpdateProgress struct {
	SubmissionStarted     bool    `json:"submission_started"`
	OldCard               nmiCard `json:"old_card"`
	StagedBillingID       string  `json:"staged_billing_id,omitempty"`
	VerificationSubmitted bool    `json:"verification_submitted,omitempty"`
	AgreementRef          string  `json:"agreement_ref,omitempty"`
	Finalized             bool    `json:"finalized,omitempty"`
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
	DB     *db.DB
	Rails  RailClientResolver
	Store  *Store
	Clock  clockwork.Clock
	Policy BackoffPolicy
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
	return h.advance(ctx, intent, false)
}

func (h *NMIPaymentMethodUpdateHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	return h.advance(ctx, intent, true)
}

// verificationOrderID is the replacement's card-verification order reference:
// a retry finds an earlier verification by it instead of verifying twice.
func verificationOrderID(intentID uuid.UUID) string { return "pmu-" + intentID.String() }

// advance drives one replacement: stage the new card in the vault, verify it
// as a recurring agreement, move the method onto it (card and agreement
// together), then retire the replaced billing entry. Each provider step is
// resumed from the durable progress; a single-use token is never resubmitted.
func (h *NMIPaymentMethodUpdateHandler) advance(ctx context.Context, intent gen.OpenrailsRailIntent, verifying bool) Outcome {
	payload, err := decodeNMIPaymentMethodUpdatePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	progress, err := decodeNMIPaymentMethodUpdateProgress(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	pm, client, outcome, ok := h.dependencies(ctx, intent, payload, progress)
	if !ok {
		return outcome
	}
	vault, oldBilling := payload.RailCustomerRef, payload.RailMethodRef
	readFailed := func(what string, err error) Outcome {
		if verifying || progress.SubmissionStarted {
			return Ambiguous(what + ": " + err.Error())
		}
		return Retryable(what + ": " + err.Error())
	}
	customer, found, err := client.GetCustomer(ctx, vault)
	if err != nil {
		return readFailed("provider vault read failed", err)
	}
	if !found {
		return Terminal("stored card vault is absent at NMI; replacement cannot apply")
	}

	if progress.Finalized {
		if _, present := billingEntry(customer, oldBilling); present {
			if err := client.DeleteCustomerBillingEntry(ctx, vault, oldBilling); err != nil {
				return Retryable("retire the replaced billing entry: " + err.Error())
			}
		}
		return Succeeded(map[string]any{"confirmation": "replacement_card_verified", "payment_method_id": pm.ID, "billing_id": progress.StagedBillingID, "intent_id": intent.ID})
	}

	old, err := entryCard(customer, oldBilling)
	if errors.Is(err, errNMICardDataGap) {
		return h.dataGap(ctx, intent, payload)
	}
	if err != nil {
		return Terminal("stored card is absent at NMI; replacement cannot apply")
	}

	staged := progress.StagedBillingID
	if !progress.SubmissionStarted {
		if verifying {
			return Terminal("card replacement is missing its durable submission boundary")
		}
		if !intent.CreatedAt.IsZero() && !h.now().Before(intent.CreatedAt.Add(collectJSTokenLifetime)) {
			return retokenizeTerminal("Collect.js token expired before the replacement could be submitted")
		}
		if h.Store == nil {
			return Parked("payment method update progress store not wired")
		}
		if err := h.Store.RecordProgress(ctx, intent.ID, map[string]any{"submission_started": true, "old_card": old}); err != nil {
			return Retryable("record card replacement submission boundary: " + err.Error())
		}
		progress.SubmissionStarted, progress.OldCard = true, old
		known := make([]string, 0, len(customer.Billing))
		for _, b := range customer.Billing {
			known = append(known, b.ID)
		}
		id, err := client.AddCustomerBillingEntry(ctx, vault, payload.providerUpdate().CreateCustomerVaultData, known)
		switch {
		case errors.Is(err, nmi.ErrProviderReadOnly):
			return Parked("nmi provider writes blocked (mode=readonly)")
		case err != nil && nmi.IsTransportAmbiguous(err):
			return Ambiguous("replacement card staging outcome unknown: " + err.Error())
		case err != nil:
			return Terminal("card replacement rejected cleanly: " + err.Error())
		}
		staged = id
		if err := h.Store.RecordProgress(ctx, intent.ID, map[string]any{"staged_billing_id": staged}); err != nil {
			return Ambiguous("record staged billing entry: " + err.Error())
		}
		if customer, found, err = client.GetCustomer(ctx, vault); err != nil || !found {
			return Ambiguous("read the vault after staging the replacement card failed")
		}
	}
	if staged == "" {
		// The staging request's outcome was lost: the entry NMI added is the
		// one carrying the requested card beside the replaced one.
		var candidates []string
		for _, b := range customer.Billing {
			if card, err := billingCard(b); err == nil && strings.TrimSpace(b.ID) != oldBilling && payload.TargetCard.matches(card) {
				candidates = append(candidates, strings.TrimSpace(b.ID))
			}
		}
		switch len(candidates) {
		case 0:
			return retokenizeTerminal("NMI still has only the original card; the single-use replacement token cannot be submitted again")
		case 1:
			staged = candidates[0]
		default:
			return Terminal("NMI holds several entries matching the replacement card; refusing to guess which was staged")
		}
		if err := h.Store.RecordProgress(ctx, intent.ID, map[string]any{"staged_billing_id": staged}); err != nil {
			return Ambiguous("record staged billing entry: " + err.Error())
		}
	}
	stagedCard, err := entryCard(customer, staged)
	if errors.Is(err, errNMICardDataGap) {
		return h.dataGap(ctx, intent, payload)
	}
	if err != nil {
		return Terminal("the staged replacement card disappeared at NMI")
	}
	if !payload.TargetCard.matches(stagedCard) {
		return TerminalWithEvidence("NMI staged a different card than the requested replacement; refusing to use it", map[string]any{"provider_card": stagedCard})
	}

	ref := progress.AgreementRef
	if ref == "" {
		order := verificationOrderID(intent.ID)
		var refused *nmi.Verification
		if progress.VerificationSubmitted {
			v, found, err := client.ReadVerificationByOrderID(ctx, order)
			if err != nil {
				return Ambiguous("read the replacement card verification: " + err.Error())
			}
			if found && v.Approved {
				ref = v.TransactionID
			} else if found {
				refused = &nmi.Verification{ResponseCode: v.ResponseCode}
			}
		}
		if ref == "" && refused == nil {
			if err := h.Store.RecordProgress(ctx, intent.ID, map[string]any{"verification_submitted": true}); err != nil {
				return Ambiguous("record verification boundary: " + err.Error())
			}
			// A verification moves no funds; one lost before NMI recorded it
			// is sent again under the same order reference.
			ref, err = client.EstablishRecurringAgreement(ctx, vault, staged, order)
			var decline *nmi.CustomerVaultError
			switch {
			case errors.Is(err, nmi.ErrProviderReadOnly):
				return Parked("nmi provider writes blocked (mode=readonly)")
			case errors.Is(err, nmi.ErrDuplicateTransaction):
				// Refused unprocessed by NMI's duplicate window; the next pass
				// finds nothing under the order and verifies again.
				return Retryable("NMI refused the replacement card verification as a duplicate; retrying after its window")
			case errors.As(err, &decline) && !nmi.UncertainResponseCode(decline.ResponseCode):
				refused = &nmi.Verification{ResponseCode: decline.ResponseCode, ResponseText: strings.TrimSpace(decline.LocalizationID)}
			case err != nil:
				return Ambiguous("replacement card verification outcome unknown: " + err.Error())
			}
		}
		if refused != nil {
			// The issuer refused the new card: it never becomes the method's
			// card, and the previous card and agreement stay in use.
			if _, present := billingEntry(customer, staged); present {
				if err := client.DeleteCustomerBillingEntry(ctx, vault, staged); err != nil {
					return Retryable("remove the refused replacement card: " + err.Error())
				}
			}
			// The customer-facing decline code: NMI's localization id when it
			// named one, else its response code.
			code := refused.ResponseText
			if code == "" {
				code = fmt.Sprintf("nmi_response_%d", refused.ResponseCode)
			}
			return TerminalWithEvidence("the card issuer refused to verify the replacement card", map[string]any{"declined": true, "decline_code": code, "response_code": refused.ResponseCode})
		}
		if err := h.Store.RecordProgress(ctx, intent.ID, map[string]any{"agreement_ref": ref}); err != nil {
			return Ambiguous("record replacement card agreement: " + err.Error())
		}
	}

	if out := h.finalize(ctx, intent, pm, oldBilling, staged, stagedCard, ref, payload.NameOnCard); out.Class != OutcomeSucceeded {
		return out
	}
	if err := client.DeleteCustomerBillingEntry(ctx, vault, oldBilling); err != nil {
		return Retryable("retire the replaced billing entry: " + err.Error())
	}
	return Succeeded(map[string]any{"confirmation": "replacement_card_verified", "payment_method_id": pm.ID, "billing_id": staged, "provider_card": stagedCard, "intent_id": intent.ID})
}

func (h *NMIPaymentMethodUpdateHandler) dependencies(ctx context.Context, intent gen.OpenrailsRailIntent, payload NMIPaymentMethodUpdatePayload, progress nmiPaymentMethodUpdateProgress) (*models.PaymentMethod, *nmi.NMIClient, Outcome, bool) {
	pm, err := paymentmethods.NewPaymentMethodRepo(h.DB).GetByID(ctx, payload.PaymentMethodID)
	if err != nil {
		if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
			return nil, nil, Terminal("payment method row no longer exists"), false
		}
		return nil, nil, Retryable("load payment method: " + err.Error()), false
	}
	billing := strings.TrimSpace(payload.RailMethodRef)
	if progress.Finalized {
		billing = progress.StagedBillingID
	}
	if pm.CustomerID.String() != payload.UserID || strings.TrimSpace(pm.RailCustomerRef) != strings.TrimSpace(payload.RailCustomerRef) ||
		strings.TrimSpace(pm.RailMethodRef) != billing {
		return nil, nil, Terminal("payment method identity changed while card replacement was pending"), false
	}
	if strings.HasPrefix(pm.ParkReason, "delete:") {
		return nil, nil, Parked("payment method deletion is pending"), false
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

// finalize moves the method onto the verified billing entry: its card, its
// recurring agreement and the durable finalized mark commit together.
func (h *NMIPaymentMethodUpdateHandler) finalize(ctx context.Context, intent gen.OpenrailsRailIntent, pm *models.PaymentMethod, oldBilling, staged string, card nmiCard, ref, nameOnCard string) Outcome {
	metadata := map[string]any{}
	for k, v := range pm.Metadata {
		metadata[k] = v
	}
	if name := strings.TrimSpace(nameOnCard); name != "" {
		metadata["name_on_card"] = name
	}
	raw, err := models.ToJSONB(metadata)
	if err != nil {
		return Terminal("encode payment method metadata: " + err.Error())
	}
	now := h.now()
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		n, err := d.Gen(ctx).ReplacePaymentMethodCard(ctx, gen.ReplacePaymentMethodCardParams{
			NewRailMethodRef: staged, LastFour: stringPtr(card.LastFour), CardType: stringPtr(card.CardType), ExpiryDate: stringPtr(card.ExpiryDate),
			Metadata: raw, RecurringRef: ref, UpdatedAt: now, MerchantID: intent.MerchantID, ID: pm.ID, OldRailMethodRef: oldBilling,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("payment method changed before the replacement card could be recorded")
		}
		if err := NewStore(d).RecordProgress(ctx, intent.ID, map[string]any{"finalized": true}); err != nil {
			return err
		}
		return subscriptions.WakeForReplacedMethod(ctx, d, intent.MerchantID, pm.ID, now)
	})
	if err != nil {
		return Ambiguous("replacement card verified, but local finalize failed: " + err.Error())
	}
	return Succeeded(nil)
}

// billingEntry finds a vault billing entry by id.
func billingEntry(customer nmi.V5Customer, billingID string) (nmi.V5CustomerBilling, bool) {
	ref := strings.TrimSpace(billingID)
	for _, b := range customer.Billing {
		if strings.TrimSpace(b.ID) == ref {
			return b, true
		}
	}
	return nmi.V5CustomerBilling{}, false
}

var errNMIBillingEntryAbsent = errors.New("billing entry absent")

// entryCard is the masked card of one billing entry ("" = the primary).
func entryCard(customer nmi.V5Customer, billingID string) (nmiCard, error) {
	if strings.TrimSpace(billingID) == "" {
		if primary := customer.PrimaryBilling(); primary != nil {
			return billingCard(*primary)
		}
		return nmiCard{}, errNMIBillingEntryAbsent
	}
	b, ok := billingEntry(customer, billingID)
	if !ok {
		return nmiCard{}, errNMIBillingEntryAbsent
	}
	return billingCard(b)
}

func billingCard(b nmi.V5CustomerBilling) (nmiCard, error) {
	card := nmiCard{
		LastFour:   maskedLastFour(b.PaymentDetails.CardNumber),
		CardType:   strings.TrimSpace(b.PaymentDetails.CardType),
		ExpiryDate: normalizeNMIExpiry(b.PaymentDetails.CardExp),
	}
	if card.CardType == "" {
		card.CardType = nmi.CardBrandFromMaskedPAN(b.PaymentDetails.CardNumber)
	}
	if !card.complete() {
		return nmiCard{}, errNMICardDataGap
	}
	return card, nil
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
		NameOnCard:      valueOrEmpty(req.NameOnCard),
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
		if intentEvidenceBool(row.ResultEvidence, "declined") {
			out.DeclineCode = intentEvidenceString(row.ResultEvidence, "decline_code")
		}
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

func intentEvidenceString(raw []byte, key string) string {
	var evidence map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &evidence) != nil {
		return ""
	}
	value, _ := evidence[key].(string)
	return value
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// dataGap ends a replacement NMI cannot describe and raises an operator
// finding; the local card keeps its last confirmed metadata.
func (h *NMIPaymentMethodUpdateHandler) dataGap(ctx context.Context, intent gen.OpenrailsRailIntent, payload NMIPaymentMethodUpdatePayload) Outcome {
	evidence, _ := json.Marshal(map[string]any{"payment_method_id": payload.PaymentMethodID.String(), "customer_vault_id": payload.RailCustomerRef, "billing_id": payload.RailMethodRef, "intent_id": intent.ID.String()})
	action := "NMI returned this stored card without its last four digits or expiry. Check the customer vault at NMI; the customer can add the card again."
	if _, err := h.DB.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: intent.MerchantID, FindingType: PaymentMethodDataGapFinding,
		SubjectKey: payload.PaymentMethodID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence}); err != nil {
		return Retryable("record card data gap finding: " + err.Error())
	}
	return Terminal(errNMICardDataGap.Error())
}
