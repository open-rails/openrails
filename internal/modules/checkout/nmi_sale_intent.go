package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

const (
	nmiSaleEvidenceTransactionID = "transaction_id"
	nmiSaleEvidencePaymentID     = "payment_id"
	nmiSaleEvidenceDelayedStart  = "delayed_start"
)

func NMISaleIdempotencyKey(key string) string {
	return payments.TypeNMISale + ":" + strings.TrimSpace(key)
}
func nmiSaleIntentOrderID(id uuid.UUID, run string) string {
	return payments.NMISaleOrderReference(id, run)
}

type NMISaleIntentHandler struct {
	Sale   *CheckoutNMISaleService
	Policy intents.BackoffPolicy
}

func NewNMISaleIntentHandler(sale *CheckoutNMISaleService) *NMISaleIntentHandler {
	return &NMISaleIntentHandler{Sale: sale, Policy: intents.DefaultBackoff}
}
func (h *NMISaleIntentHandler) Type() string                         { return payments.TypeNMISale }
func (h *NMISaleIntentHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }
func (h *NMISaleIntentHandler) PrunePolicy() (bool, bool)            { return true, true }
func (h *NMISaleIntentHandler) CommitsTerminalOutcome() bool         { return true }
func (h *NMISaleIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *NMISaleIntentHandler) database() *db.DB {
	if h.Sale == nil || h.Sale.RailPaymentMethodService == nil {
		return nil
	}
	return h.Sale.RailPaymentMethodService.DB
}

func (h *NMISaleIntentHandler) Execute(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	if h.database() == nil || h.Sale.PurchaseService == nil {
		return intents.Parked("checkout sale service not wired")
	}
	current, err := intents.NewStore(h.database()).Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous("cannot load accepted sale: " + err.Error())
	}
	var progress map[string]any
	if len(current.ResultEvidence) > 0 {
		if err := json.Unmarshal(current.ResultEvidence, &progress); err != nil {
			return intents.Ambiguous("invalid canonical sale progress")
		}
	}
	if progress["sale_submitted"] == true || current.Status == intents.StatusSucceeded || current.Status == intents.StatusFailedTerminal {
		return h.Verify(ctx, current)
	}
	in = current
	p, err := payments.DecodeNMISalePayload(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	client, err := h.Sale.nmiClient(db.WithPSPID(ctx, *in.PspID), nmiIntentClientName(p.PSP, in.Rail))
	if err != nil || client == nil {
		return intents.Parked("accepted NMI account cannot be armed")
	}
	owner, account := client.AccountIdentity()
	if owner != in.MerchantID || account != *in.PspID {
		return intents.Parked("NMI client does not identify the accepted account")
	}
	if client.ReadOnly {
		return intents.Parked("nmi client is read-only")
	}
	err = h.database().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		method, err := h.database().NewWithPgxTx(tx).Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID.String() != p.UserID || method.ParkReason != "" {
			return errors.New("sale method changed before submission")
		}
		return p.Instrument.Matches(method, charge.AgreementUnscheduled)
	})
	if err != nil {
		return h.complete(ctx, in, nil, intents.TerminalWithEvidence("instrument changed before submission", map[string]any{"not_executed": true}))
	}
	submitted, err := intents.NewStore(h.database()).RecordProgressIfAbsent(ctx, in.ID, "sale_submitted", true)
	if err != nil {
		return intents.Ambiguous("cannot retain sale submission fence: " + err.Error())
	}
	if !submitted {
		return h.Verify(ctx, in)
	}
	minor, err := moneyutil.NativeToRailMinorExact(p.Currency, p.Amount)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	credential := charge.InitialOneTime()
	if p.Instrument.StoredCredentialUnscheduledRef != "" {
		credential = charge.OneTimeReuse(p.Instrument.StoredCredentialUnscheduledRef)
	}
	response, callErr := client.RunSale(ctx, nmi.SaleParams{CustomerVaultID: p.Instrument.RailCustomerRef, BillingID: p.Instrument.RailMethodRef, Amount: minor, Currency: p.Currency, OrderDescription: p.Description, OrderID: payments.NMISaleOrderReference(in.ID, p.E2ERunID), StoredCredential: nmidirect.StoredCredentialFor(credential)})
	if callErr != nil {
		if nmi.RequiresVerification(callErr) {
			return intents.Ambiguous("sale outcome requires provider verification")
		}
		var refusal *nmi.CustomerVaultError
		evidence := map[string]any{"request_refused": true}
		reason := "provider refused the sale request"
		if errors.As(callErr, &refusal) {
			evidence = map[string]any{"declined": true, "response_code": refusal.ResponseCode, "localization_id": refusal.LocalizationID}
			reason = "sale declined"
		}
		if err := intents.NewStore(h.database()).RecordProgress(ctx, in.ID, evidence); err != nil {
			return intents.Ambiguous("cannot retain sale refusal: " + err.Error())
		}
		return h.complete(ctx, in, nil, intents.TerminalWithEvidence(reason, evidence))
	}
	if response == nil || response.TransactionID == "" {
		return intents.Ambiguous("sale response has no transaction candidate")
	}
	return h.collect(ctx, in, client, response.TransactionID)
}

func (h *NMISaleIntentHandler) Verify(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	if h.database() == nil || h.Sale.PurchaseService == nil {
		return intents.Ambiguous("sale recovery service is unavailable")
	}
	current, err := intents.NewStore(h.database()).Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	receipt, found, err := intents.LoadCollectedReceipt(current)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if found {
		return h.complete(ctx, current, &receipt, intents.Succeeded(nil))
	}
	var evidence map[string]any
	if len(current.ResultEvidence) > 0 && json.Unmarshal(current.ResultEvidence, &evidence) != nil {
		return intents.Ambiguous("invalid sale progress")
	}
	if evidence["declined"] == true || evidence["not_executed"] == true || evidence["request_refused"] == true {
		return h.complete(ctx, current, nil, intents.TerminalWithEvidence("sale refused", evidence))
	}
	p, err := payments.DecodeNMISalePayload(current)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if evidence["sale_submitted"] != true {
		// A crashed claim is not a submitted payment. Re-enter the executor
		// through its write gates; verification itself never sends money.
		return intents.Retryable("accepted sale has not been submitted")
	}
	client, err := h.Sale.nmiClient(db.WithPSPID(ctx, *current.PspID), nmiIntentClientName(p.PSP, current.Rail))
	if err != nil || client == nil {
		return intents.Ambiguous("accepted NMI account cannot be read")
	}
	return h.collect(ctx, current, client, intents.EvidenceString(current, "transaction_id"))
}

func (h *NMISaleIntentHandler) collect(ctx context.Context, in gen.OpenrailsRailIntent, client *nmi.NMIClient, reference string) intents.Outcome {
	receipt, found, err := intents.ReadNMICollectionReceipt(ctx, in, upgradeReceiptResolver{client}, reference)
	if err != nil || !found {
		return intents.Ambiguous("sale has no qualified exact payment receipt")
	}
	receipt, err = intents.NewStore(h.database()).RetainCollectedReceipt(ctx, in, receipt)
	if err != nil {
		return intents.Ambiguous("cannot retain sale receipt: " + err.Error())
	}
	return h.complete(ctx, in, &receipt, intents.Succeeded(nil))
}

func (h *NMISaleIntentHandler) Resolve(ctx context.Context, in gen.OpenrailsRailIntent, resolution intents.Resolution) (intents.Outcome, error) {
	if h.database() == nil || h.Sale.PurchaseService == nil {
		return intents.Outcome{}, intents.RejectResolution("sale recovery service is unavailable")
	}
	if resolution.Step != "" {
		return intents.Outcome{}, intents.RejectResolution("sale has no steps")
	}
	if resolution.NotExecuted {
		return intents.Outcome{}, intents.RejectResolution("post-submission sale nonexecution is not provable from search absence")
	}
	p, err := payments.DecodeNMISalePayload(in)
	if err != nil {
		return intents.Outcome{}, err
	}
	client, err := h.Sale.nmiClient(db.WithPSPID(ctx, *in.PspID), nmiIntentClientName(p.PSP, in.Rail))
	if err != nil {
		return intents.Outcome{}, err
	}
	receipt, found, err := intents.ReadNMICollectionReceipt(ctx, in, upgradeReceiptResolver{client}, resolution.ProviderReference)
	if err != nil || !found {
		return intents.Outcome{}, intents.RejectResolution("exact sale receipt is unavailable or contradicts accepted terms")
	}
	receipt, err = intents.NewStore(h.database()).RetainCollectedReceipt(ctx, in, receipt)
	if err != nil {
		return intents.Outcome{}, err
	}
	return h.complete(ctx, in, &receipt, intents.Succeeded(nil)), nil
}

func (h *NMISaleIntentHandler) complete(ctx context.Context, in gen.OpenrailsRailIntent, receipt *intents.CollectedReceipt, outcome intents.Outcome) intents.Outcome {
	p, err := payments.DecodeNMISalePayload(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	ctx, cancel := intents.LedgerWriteContext(ctx)
	defer cancel()
	ctx = db.WithPSPID(ctx, *in.PspID)
	err = h.database().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.database().NewWithPgxTx(tx)
		customer, _ := uuid.Parse(p.UserID)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: customer}); err != nil {
			return err
		}
		current, err := d.Gen(ctx).LockRailIntentForSaleCompletion(ctx, gen.LockRailIntentForSaleCompletionParams{MerchantID: in.MerchantID, ID: in.ID})
		if err != nil {
			return err
		}
		if current.PspID == nil || *current.PspID != *in.PspID || current.IdempotencyKey != in.IdempotencyKey || !bytes.Equal(current.Payload, in.Payload) {
			return errors.New("sale completion contradicts accepted operation")
		}
		retained, paid, err := intents.LoadCollectedReceipt(current)
		if err != nil {
			return err
		}
		success := outcome.Class == intents.OutcomeSucceeded
		if success {
			if !paid || receipt == nil || receipt.Validate(current) != nil || retained.TransactionID() != receipt.TransactionID() {
				return errors.New("sale completion has no matching retained receipt")
			}
		} else if paid {
			return errors.New("paid sale cannot be refused")
		}
		status := intents.StatusFailedTerminal
		if success {
			status = intents.StatusSucceeded
		}
		var evidence map[string]any
		if len(current.ResultEvidence) > 0 {
			if err := json.Unmarshal(current.ResultEvidence, &evidence); err != nil {
				return err
			}
		}
		if evidence == nil {
			evidence = map[string]any{}
		}
		if paid && (evidence["declined"] == true || evidence["not_executed"] == true || evidence["request_refused"] == true) {
			return errors.New("qualified sale payment contradicts refusal metadata")
		}
		if !success && outcome.Evidence["not_executed"] == true && evidence["sale_submitted"] == true {
			return errors.New("submitted sale cannot become pre-send nonexecution")
		}
		if current.Status == intents.StatusSucceeded || current.Status == intents.StatusFailedTerminal {
			if current.Status != status {
				return errors.New("sale terminal replay conflicts")
			}
			outcome.Evidence = saleResultEvidence(evidence)
			return nil
		}
		purchase := h.Sale.PurchaseService.transactionBound(d)
		now := purchase.now().UTC()
		if success {
			price := &models.Price{ID: p.PriceID, ProductID: p.ProductID, Amount: p.ListAmount, Currency: p.Currency, AccessDurationHours: p.AccessDurationHours}
			product := &models.Product{ID: p.ProductID, EntitlementsSpec: models.CloneEntitlementsSpec(p.Entitlements)}
			eligibility := &EligibilityResult{Status: EligibilityStatus(p.Eligibility), Coverage: &CoverageInfo{}}
			if p.EntitlementStart.After(p.AcceptedAt) {
				eligibility.Coverage = &CoverageInfo{HasCoverage: true, EndDate: &p.EntitlementStart}
			}
			metadata := map[string]any{"order_id": payments.NMISaleOrderReference(in.ID, p.E2ERunID)}
			if p.E2ERunID != "" {
				metadata["e2e_run_id"] = p.E2ERunID
			}
			result, err := purchase.applyPurchase(ctx, &payments.RegisterPurchaseRequest{UserID: p.UserID, PriceID: p.PriceID, Rail: "nmi", TransactionID: receipt.TransactionID(), Amount: p.Amount, AmountProvided: true, Currency: p.Currency, PurchasedAt: &p.AcceptedAt, Metadata: metadata, AttemptKind: payments.AttemptInitial, TokenType: charge.TokenTypePSPToken}, price, product, eligibility, p.AcceptedAt, p.PaymentID)
			if err != nil {
				return err
			}
			if _, err := d.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID, Agreement: "unscheduled", Ref: receipt.TransactionID()}); err != nil {
				return err
			}
			evidence["transaction_id"], evidence["payment_id"] = receipt.TransactionID(), result.PaymentID.String()
			if result.DelayedStart != nil {
				evidence["delayed_start"] = result.DelayedStart.UTC().Format(time.RFC3339Nano)
			}
		} else {
			for key, value := range outcome.Evidence {
				evidence[key] = value
			}
			if evidence["declined"] == true {
				code, _ := evidence["localization_id"].(string)
				reason := payments.NormalizeFailureReason("nmi", code)
				kind, token := payments.AttemptInitial, charge.TokenTypePSPToken
				_, err := purchase.PaymentService.CreateIfNotExists(ctx, &models.Payment{ID: p.PaymentID, CustomerID: customer, PriceID: p.PriceID, Rail: "nmi", TransactionID: "nmi_sale_declined:" + in.ID.String(), Amount: p.Amount, ListAmount: p.ListAmount, Currency: p.Currency, Status: payments.PaymentStatusFailedValue, AttemptKind: &kind, TokenType: &token, FailureCode: &code, FailureReason: &reason, MoneyMovement: models.MoneyMovementNone, PurchasedAt: p.AcceptedAt, CreatedAt: now})
				if err != nil {
					return err
				}
			}
		}
		if record := intents.OperatorResolutionRecord(ctx); record != nil {
			evidence["operator_resolution"] = record
		}
		raw, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		reason := outcome.Reason
		n, err := d.Gen(ctx).CompleteSaleOutcome(ctx, gen.CompleteSaleOutcomeParams{MerchantID: in.MerchantID, ID: in.ID, Status: status, Evidence: raw, Reason: &reason, Now: now})
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("sale terminal outcome did not commit")
		}
		outcome.Evidence = saleResultEvidence(evidence)
		return nil
	})
	if err != nil {
		return intents.Ambiguous("sale receipt retained; local completion pending: " + err.Error())
	}
	return outcome
}

// refuseContradictedNonExecution rejects a non-execution attestation while the
// provider shows a successful sale on the operation's exact order reference.
func refuseContradictedNonExecution(ctx context.Context, client *nmi.NMIClient, orderID string) error {
	txn, found, err := client.FindSuccessfulSaleByOrderID(ctx, orderID)
	if err != nil {
		return fmt.Errorf("read provider order before accepting non-execution: %w", err)
	}
	if found {
		return intents.RejectResolution("provider shows successful sale %s for order %s", txn, orderID)
	}
	return nil
}

// The persisted row retains custody. Runner diagnostics/pruning receive only
// the result projection, never a caller-writable copy of reserved receipt keys.
func saleResultEvidence(evidence map[string]any) map[string]any {
	out := make(map[string]any, len(evidence))
	for key, value := range evidence {
		if key != "qualified_receipt" && key != "qualified_enrollment" && key != "account_requalifications" {
			out[key] = value
		}
	}
	return out
}
