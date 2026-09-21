package intents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const qualifiedReceiptKey = "qualified_receipt"

// CollectedReceipt is provider evidence qualified against the accepted operation.
// Its zero value is invalid; no caller can construct one from a transaction ID
// or a boolean. Load revalidates every field against the same canonical payload.
type CollectedReceipt struct{ data collectedReceipt }

type receiptBinding struct {
	OperationID   uuid.UUID `json:"operation_id"`
	MerchantID    uuid.UUID `json:"merchant_id"`
	PSPID         uuid.UUID `json:"psp_id"`
	Kind          string    `json:"kind"`
	PayloadSHA256 string    `json:"payload_sha256"`
}
type collectedReceipt struct {
	Version int                                    `json:"version"`
	Family  string                                 `json:"family"`
	Binding receiptBinding                         `json:"binding"`
	NMI     *nmi.SaleEvidence                      `json:"nmi,omitempty"`
	Stripe  *subscriptions.StripeCollectionReceipt `json:"stripe,omitempty"`
}

// Each kind decodes its own accepted payload. These private expected fields
// are never an API that a caller can supply alongside a fabricated receipt.
type collectedTerms struct {
	Rail                string
	Currency            string
	AmountMinor         moneyutil.Cents
	Instrument          charge.FrozenInstrument
	ProviderCustomerRef string
	OrderReference      string
}

func decodeCollectedTerms(in gen.OpenrailsRailIntent) (collectedTerms, error) {
	switch in.IntentType {
	case payments.TypeNMISale:
		p, err := payments.DecodeNMISalePayload(in)
		if err != nil {
			return collectedTerms{}, err
		}
		minor, err := moneyutil.NativeToRailMinorExact(p.Currency, p.Amount)
		return collectedTerms{"nmi", p.Currency, minor, p.Instrument, "", payments.NMISaleOrderReference(in.ID, p.E2ERunID)}, err

	case "invoice_collection":
		p, err := DecodeInvoiceCollectionPayload(in)
		return collectedTerms{p.Rail, p.Currency, p.AmountMinor, p.Instrument, p.ProviderCustomerRef, in.ID.String()}, err
	case subscriptions.TypeNMIInitialEnrollment:
		p, err := subscriptions.DecodeNMIInitialEnrollmentPayload(in)
		if err != nil {
			return collectedTerms{}, err
		}
		minor, err := moneyutil.NativeToRailMinorExact(p.Currency, p.AmountMicros)
		return collectedTerms{"nmi", p.Currency, minor, p.Instrument, "", payments.NMISaleOrderReference(in.ID, p.E2ERunID)}, err
	case subscriptions.TypeNMIUpgrade:
		p, err := subscriptions.DecodeNMIUpgradePayload(in)
		if err != nil {
			return collectedTerms{}, err
		}
		minor, err := moneyutil.NativeToRailMinorExact(p.Currency, p.ProrationAmount)
		return collectedTerms{"nmi", p.Currency, minor, p.Instrument, "", in.ID.String()}, err
	case subscriptions.TypeManualRebill:
		p, err := subscriptions.DecodeManualRebillPayload(in)
		return collectedTerms{p.Rail, p.Renewal.Currency, p.AmountMinor, p.Instrument, "", p.OrderReference}, err
	default:
		return collectedTerms{}, errors.New("operation kind has no collected-receipt contract")
	}
}

func collectionBinding(in gen.OpenrailsRailIntent) (receiptBinding, error) {
	if _, err := decodeCollectedTerms(in); err != nil {
		return receiptBinding{}, err
	}
	d := json.NewDecoder(bytes.NewReader(in.Payload))
	d.UseNumber()
	var payload any
	if err := d.Decode(&payload); err != nil {
		return receiptBinding{}, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return receiptBinding{}, errors.New("operation payload has trailing data")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return receiptBinding{}, err
	}
	digest := sha256.Sum256(raw)
	return receiptBinding{in.ID, in.MerchantID, *in.PspID, in.IntentType, hex.EncodeToString(digest[:])}, nil
}

// ReadNMICollectionReceipt resolves the accepted PSP and reads the actual order
// match and exact transaction from that account. Terms are never caller inputs.
func ReadNMICollectionReceipt(ctx context.Context, in gen.OpenrailsRailIntent, resolver NMIClientResolver, reference string) (CollectedReceipt, bool, error) {
	binding, err := collectionBinding(in)
	if err != nil {
		return CollectedReceipt{}, false, err
	}
	p, _ := decodeCollectedTerms(in)
	if p.Rail == "stripe" {
		return CollectedReceipt{}, false, errors.New("Stripe operation cannot accept an NMI receipt")
	}
	client, err := resolveReceiptNMIClient(ctx, resolver, in)
	if err != nil {
		return CollectedReceipt{}, false, err
	}
	facts, found, err := client.ReadSaleEvidence(ctx, p.OrderReference, reference)
	if err != nil || !found {
		return CollectedReceipt{}, found, err
	}
	receipt := CollectedReceipt{collectedReceipt{Version: 1, Family: "collected_payment", Binding: binding, NMI: &facts}}
	return receipt, true, receipt.Validate(in)
}

// ReadStripeCollectionReceipt accepts an account-scoped reader, whose PSP must
// be the immutable account resolved by the credential plane. It never accepts
// caller-supplied amount/customer terms or a boolean settled verdict.
func ReadStripeCollectionReceipt(ctx context.Context, in gen.OpenrailsRailIntent, service *subscriptions.StripeService, reference string) (CollectedReceipt, bool, error) {
	binding, err := collectionBinding(in)
	if err != nil {
		return CollectedReceipt{}, false, err
	}
	p, _ := decodeCollectedTerms(in)
	if p.Rail != "stripe" || service == nil {
		return CollectedReceipt{}, false, errors.New("Stripe receipt reader does not match accepted provider account")
	}
	accountMerchant, accountPSP := service.AccountIdentity()
	if accountMerchant != in.MerchantID || accountPSP != *in.PspID {
		return CollectedReceipt{}, false, errors.New("Stripe reader is armed for another provider account")
	}
	facts, found, err := service.GetCollectedInvoice(ctx, reference)
	if err != nil || !found {
		return CollectedReceipt{}, found, err
	}
	receipt := CollectedReceipt{collectedReceipt{Version: 1, Family: "collected_payment", Binding: binding, Stripe: &facts}}
	return receipt, true, receipt.Validate(in)
}

func (r CollectedReceipt) Validate(in gen.OpenrailsRailIntent) error {
	binding, err := collectionBinding(in)
	if err != nil {
		return err
	}
	if r.data.Version != 1 || r.data.Family != "collected_payment" || r.data.Binding != binding {
		return errors.New("qualified collection receipt is not bound to this accepted operation")
	}
	p, _ := decodeCollectedTerms(in)
	if p.Rail == "stripe" {
		if r.data.Stripe == nil || r.data.NMI != nil {
			return errors.New("qualified collection receipt has wrong provider family")
		}
		facts := r.data.Stripe
		if err := facts.MatchesOperation(in.ID.String(), p.AmountMinor, p.Currency); err != nil {
			return err
		}
		if facts.ChargeID == "" || !facts.ChargePaid || !facts.ChargeCaptured || facts.ChargeStatus != "succeeded" || facts.ChargedAmount != int64(p.AmountMinor) || facts.ChargeCurrency != p.Currency || facts.ChargeCustomerID != p.ProviderCustomerRef {
			return errors.New("Stripe captured charge does not match frozen collection")
		}
		if (facts.PaymentIntentID != "" && facts.ChargePaymentIntentID != facts.PaymentIntentID) || (facts.ChargeInvoiceID != "" && facts.ChargeInvoiceID != facts.InvoiceID) || (facts.PaymentIntentID == "" && facts.ChargeInvoiceID == "") {
			return errors.New("captured Stripe charge is not linked to this invoice payment")
		}
		if facts.InvoiceID == "" || facts.CustomerID != p.ProviderCustomerRef || facts.PaymentMethodID != p.Instrument.RailMethodRef {
			return errors.New("Stripe receipt does not match frozen customer and payment method")
		}
	} else {
		if r.data.NMI == nil || r.data.Stripe != nil {
			return errors.New("qualified collection receipt has wrong provider family")
		}
		facts := r.data.NMI
		if facts.TransactionID == "" || facts.OrderReference != p.OrderReference || !facts.Approved || facts.Amount != p.AmountMinor || !strings.EqualFold(facts.Currency, p.Currency) {
			return fmt.Errorf("%w: sale does not match frozen operation", nmi.ErrReceiptMismatch)
		}
		if !p.Instrument.CustodianHeld() && (p.Instrument.RailCustomerRef == "" || facts.CustomerVaultID != p.Instrument.RailCustomerRef) {
			return fmt.Errorf("%w: sale does not match frozen vault", nmi.ErrReceiptMismatch)
		}
	}
	return nil
}

func (r CollectedReceipt) TransactionID() string {
	if r.data.NMI != nil {
		return r.data.NMI.TransactionID
	}
	if s := r.data.Stripe; s != nil {
		return s.ChargeID
	}
	return ""
}
func (r CollectedReceipt) ExternalInvoiceID() string {
	if r.data.Stripe != nil {
		return r.data.Stripe.InvoiceID
	}
	return ""
}

func LoadCollectedReceipt(in gen.OpenrailsRailIntent) (CollectedReceipt, bool, error) {
	var evidence map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return CollectedReceipt{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return CollectedReceipt{}, false, err
	}
	raw, ok := evidence[qualifiedReceiptKey]
	if !ok {
		return CollectedReceipt{}, false, nil
	}
	var r CollectedReceipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r.data); err != nil {
		return r, true, err
	}
	return r, true, r.Validate(in)
}

// RetainCollectedReceipt commits custody before local effects. A transaction-
// bound store is refused: a receipt rolled back with failed local settlement
// would leave the next worker dependent on another provider read.
func (s *Store) RetainCollectedReceipt(ctx context.Context, in gen.OpenrailsRailIntent, receipt CollectedReceipt) (CollectedReceipt, error) {
	if err := receipt.Validate(in); err != nil {
		return CollectedReceipt{}, err
	}
	current, err := s.retainQualifiedEvidence(ctx, in, qualifiedReceiptKey, receipt.data)
	if err != nil {
		return CollectedReceipt{}, err
	}
	loaded, found, err := LoadCollectedReceipt(current)
	if err != nil {
		return CollectedReceipt{}, err
	}
	if !found {
		return CollectedReceipt{}, errors.New("receipt custody disappeared before settlement")
	}
	return loaded, loaded.Validate(in)
}

// retainQualifiedEvidence is the single durable writer for sealed payment and
// enrollment receipts. Only their typed entry points call it, after validation.
// It never shares the domain transaction and never overwrites a different proof.
func (s *Store) retainQualifiedEvidence(ctx context.Context, in gen.OpenrailsRailIntent, key string, proof any) (gen.OpenrailsRailIntent, error) {
	var empty gen.OpenrailsRailIntent
	if s == nil || s.db == nil || s.db.Pool() == nil {
		return empty, errors.New("receipt custody requires the base database, outside a transaction")
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return empty, err
	}
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	result, err := s.db.Gen(ctx).RetainRailIntentQualifiedEvidence(ctx, gen.RetainRailIntentQualifiedEvidenceParams{ID: in.ID, MerchantID: in.MerchantID, PspID: *in.PspID, IntentType: in.IntentType, Payload: in.Payload, EvidenceKey: key, Receipt: raw})
	if err != nil {
		return empty, err
	}
	current, err := s.Get(ctx, in.ID)
	if err != nil {
		return empty, err
	}
	if result != 1 && current.Status != StatusSucceeded {
		return empty, errors.New("receipt custody refused stale, terminal, changed or conflicting operation")
	}
	var evidence map[string]json.RawMessage
	if err := json.Unmarshal(current.ResultEvidence, &evidence); err != nil {
		return empty, err
	}
	// PostgreSQL JSONB changes whitespace/key ordering. Compare canonical JSON
	// with UseNumber so a large integer cannot round during the custody check.
	want, err := canonicalReceiptJSON(raw)
	if err != nil {
		return empty, err
	}
	have, err := canonicalReceiptJSON(evidence[key])
	if err != nil || !bytes.Equal(want, have) {
		return empty, errors.New("retained receipt differs from the qualified proof")
	}
	return current, nil
}

func canonicalReceiptJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("receipt JSON has trailing data")
	}
	return json.Marshal(value)
}

const collectionCandidateKey = "collection_candidate"

type CollectionCandidate struct {
	TransactionID     string `json:"transaction_id"`
	ExternalInvoiceID string `json:"external_invoice_id,omitempty"`
}

// RetainCollectionCandidate preserves a possible provider object, not proof of
// payment. Replaying it always performs qualification before any local effect.
func (s *Store) RetainCollectionCandidate(ctx context.Context, in gen.OpenrailsRailIntent, candidate CollectionCandidate) error {
	if _, err := collectionBinding(in); err != nil {
		return err
	}
	if candidate.TransactionID == "" {
		return errors.New("collection candidate requires a provider reference")
	}
	if s == nil || s.db == nil || s.db.Pool() == nil {
		return errors.New("candidate custody requires the base database")
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	result, err := s.db.Gen(ctx).RetainRailIntentCollectionCandidate(ctx, gen.RetainRailIntentCollectionCandidateParams{ID: in.ID, MerchantID: in.MerchantID, PspID: *in.PspID, IntentType: in.IntentType, Payload: in.Payload, Candidate: raw})
	if err != nil {
		return err
	}
	if result != 1 {
		return errors.New("candidate custody refused stale or conflicting operation")
	}
	return nil
}

func LoadCollectionCandidate(in gen.OpenrailsRailIntent) (CollectionCandidate, bool, error) {
	var evidence map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return CollectionCandidate{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return CollectionCandidate{}, false, err
	}
	raw, ok := evidence[collectionCandidateKey]
	if !ok {
		return CollectionCandidate{}, false, nil
	}
	var candidate CollectionCandidate
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return candidate, true, err
	}
	if candidate.TransactionID == "" {
		return candidate, true, errors.New("stored collection candidate has no reference")
	}
	return candidate, true, nil
}

func refuseCustodyKeys(evidence map[string]any) error {
	for _, key := range []string{qualifiedReceiptKey, qualifiedEnrollmentKey, collectionCandidateKey, rebillPreparationKey, rebillDeclineKey, "account_requalifications"} {
		if _, ok := evidence[key]; ok {
			return fmt.Errorf("%s is reserved for immutable provider evidence custody", key)
		}
	}
	return nil
}
