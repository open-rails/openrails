package intents

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TypeNMICardVault vaults a card the server itself received (card_entry:
// server, #1129). The card is never stored, so the write cannot be replayed:
// the request that carried the card sends it once, and every later look at
// the operation settles it by reading the vault the intent names. A card
// whose vaulting cannot be established is entered again.
const TypeNMICardVault = "nmi_card_vault"

// cardVaultSettle is how long a vault the gateway does not show may still be
// arriving: past the gateway's own mutation deadline. cardVaultAttendance is
// how long the request that carried the card has the operation to itself
// before a worker looks at it; a worker has no card and can only settle.
const (
	cardVaultSettle     = 2 * time.Minute
	cardVaultAttendance = time.Minute
)

func NMICardVaultIdempotencyKey(paymentMethodID uuid.UUID) string {
	return TypeNMICardVault + ":" + paymentMethodID.String()
}

// CardVaultID and CardBillingID name the vault and the billing entry a
// card-entry operation creates. They derive from the operation, so the gateway
// can be asked about them without the card. Digits only, as the gateway's own
// ids are.
func CardVaultID(operation uuid.UUID) string   { return derivedGatewayID("vault", operation) }
func CardBillingID(operation uuid.UUID) string { return derivedGatewayID("billing", operation) }

func derivedGatewayID(kind string, operation uuid.UUID) string {
	sum := sha256.Sum256([]byte("openrails:nmi-card:" + kind + ":" + operation.String()))
	return strconv.FormatUint(100_000_000_000_000_000+binary.BigEndian.Uint64(sum[:8])%900_000_000_000_000_000, 10)
}

// NMICardVaultPayload is all the intent keeps of the card: which one it was.
type NMICardVaultPayload struct {
	UserID          string    `json:"user_id"`
	PaymentMethodID uuid.UUID `json:"payment_method_id"`
	Card            nmiCard   `json:"card"`
}

type cardVaultBillingKey struct{}

// withCardVaultBilling carries the billing identity sent beside the card to
// the run that sends it; like the card, it is not part of the intent.
func withCardVaultBilling(ctx context.Context, billing nmi.CreateCustomerVaultData) context.Context {
	return context.WithValue(ctx, cardVaultBillingKey{}, billing)
}

// CardVaultClients arms the NMI client of the PSP a card is vaulted in.
type CardVaultClients interface {
	ResolveClientForPSP(ctx context.Context, rail string, pspID uuid.UUID) (*nmi.NMIClient, error)
}

type NMICardVaultHandler struct {
	DB      *db.DB
	Clients CardVaultClients
	Store   *Store
	Clock   clockwork.Clock
	Policy  BackoffPolicy
}

func NewNMICardVaultHandler(d *db.DB, clients CardVaultClients, store *Store, clock clockwork.Clock) *NMICardVaultHandler {
	return &NMICardVaultHandler{DB: d, Clients: clients, Store: store, Clock: clock, Policy: DefaultBackoff}
}

func (h *NMICardVaultHandler) Type() string                         { return TypeNMICardVault }
func (h *NMICardVaultHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }
func (h *NMICardVaultHandler) PruneTerminalPayload() bool           { return true }
func (h *NMICardVaultHandler) Execute(ctx context.Context, intent gen.BillingRailIntent) Outcome {
	return h.advance(ctx, intent, false)
}
func (h *NMICardVaultHandler) Verify(ctx context.Context, intent gen.BillingRailIntent) Outcome {
	return h.advance(ctx, intent, true)
}
func (h *NMICardVaultHandler) CheckRelevance(context.Context, gen.BillingRailIntent) (Relevance, error) {
	return StillRelevant(), nil
}

func (h *NMICardVaultHandler) now() time.Time {
	if h.Clock != nil {
		return h.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

// reenterCard ends a vaulting that cannot be established or repeated.
func reenterCard(reason string) Outcome { return retokenizeTerminal(reason) }

func (h *NMICardVaultHandler) advance(ctx context.Context, intent gen.BillingRailIntent, verifying bool) Outcome {
	var payload NMICardVaultPayload
	if err := json.Unmarshal(intent.Payload, &payload); err != nil || payload.PaymentMethodID == uuid.Nil || !payload.Card.complete() {
		return Terminal("nmi card vault payload is incomplete")
	}
	var progress struct {
		SubmittedAt *time.Time `json:"submitted_at"`
	}
	if len(intent.ResultEvidence) > 0 {
		if err := json.Unmarshal(intent.ResultEvidence, &progress); err != nil {
			return Terminal("decode nmi card vault progress: " + err.Error())
		}
	}
	if h.Clients == nil || intent.PspID == nil {
		return Parked("card vault client resolver not wired")
	}
	client, err := h.Clients.ResolveClientForPSP(ctx, intent.Rail, *intent.PspID)
	if err != nil {
		return Parked(fmt.Sprintf("nmi client not configured for provider %q: %v", intent.Rail, err))
	}
	if client.ReadOnly {
		return Parked("nmi client is read-only (mode=readonly)")
	}
	vault, billingEntry := CardVaultID(intent.ID), CardBillingID(intent.ID)

	// The card rides the request that carried it. A run without one (a worker
	// resuming the operation) has nothing to send and only settles.
	card := cardguard.CardFrom(ctx)
	billing, _ := ctx.Value(cardVaultBillingKey{}).(nmi.CreateCustomerVaultData)
	attended := card != nil
	uncertain := false
	if progress.SubmittedAt == nil {
		if verifying {
			return Terminal("card vault is missing its durable submission boundary")
		}
		if !attended {
			return reenterCard("the request that carried the card ended before it was sent")
		}
		if h.Store == nil {
			card.Zero()
			return Parked("card vault progress store not wired")
		}
		submitted := h.now()
		if err := h.Store.RecordProgress(ctx, intent.ID, map[string]any{"submitted_at": submitted}); err != nil {
			card.Zero()
			return Retryable("record card vault submission boundary: " + err.Error())
		}
		progress.SubmittedAt = &submitted
		created, err := client.CreateCustomerVaultFromCard(ctx, vault, billingEntry, billing, card)
		var refused *nmi.CustomerVaultError
		switch {
		case err == nil && created.CustomerVaultID == vault:
			return Succeeded(map[string]any{"vault_id": vault, "billing_id": billingEntry, "intent_id": intent.ID})
		case err == nil:
			// The gateway named the vault itself, so a lost answer could never
			// be settled by a read: server card entry is unsafe on it.
			_ = client.DeleteCustomerVault(ctx, nmi.DeleteCustomerVaultData{CustomerVaultID: created.CustomerVaultID})
			return Terminal("the gateway did not keep the requested customer vault id; server card entry cannot be used with it")
		case errors.Is(err, nmi.ErrProviderReadOnly):
			return Parked("nmi provider writes blocked (mode=readonly)")
		case nmi.IsTransportAmbiguous(err):
			uncertain = true
		case errors.As(err, &refused):
			return TerminalWithEvidence("the gateway refused the card: "+refused.Error(), map[string]any{"response_code": refused.ResponseCode})
		default:
			return Terminal("card vault request was not sent: " + err.Error())
		}
	}

	_, found, err := client.GetCustomer(ctx, vault)
	if err != nil {
		return Ambiguous("read the card vault: " + err.Error())
	}
	switch {
	case found && attended:
		return Succeeded(map[string]any{"vault_id": vault, "billing_id": billingEntry, "confirmation": "vault_read", "intent_id": intent.ID})
	case found:
		// The request that carried the card is over and was told the outcome
		// is unknown. Nothing refers to this vault, so the card does not stay.
		saved, err := h.methodSaved(ctx, payload.PaymentMethodID)
		if err != nil {
			return Ambiguous("read the saved payment method: " + err.Error())
		}
		if saved {
			return Succeeded(map[string]any{"vault_id": vault, "billing_id": billingEntry, "confirmation": "payment_method_saved", "intent_id": intent.ID})
		}
		if err := client.DeleteCustomerVault(ctx, nmi.DeleteCustomerVaultData{CustomerVaultID: vault}); err != nil {
			return Ambiguous("remove the unreferenced card vault: " + err.Error())
		}
		return reenterCard("the card was vaulted after its request ended; the vault was removed and the card is entered again")
	case uncertain, h.now().Before(progress.SubmittedAt.Add(cardVaultSettle)):
		return Ambiguous("card vault outcome unknown: the gateway does not show the vault yet")
	}
	return reenterCard("the gateway never vaulted the card; it is entered again")
}

func (h *NMICardVaultHandler) methodSaved(ctx context.Context, id uuid.UUID) (bool, error) {
	_, err := paymentmethods.NewPaymentMethodRepo(h.DB).GetByID(ctx, id)
	if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
		return false, nil
	}
	return err == nil, err
}

// CardVaultThrough posts the card-vault intent and executes it in this request.
type CardVaultThrough struct{ Runner *Runner }

func (t *CardVaultThrough) ExecuteCardVault(ctx context.Context, req paymentmethods.CardVaultRequest) (paymentmethods.CardVaultOutcome, error) {
	var out paymentmethods.CardVaultOutcome
	if t == nil || t.Runner == nil {
		req.Card.Zero()
		return out, errors.New("card vault intent runner not wired")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		req.Card.Zero()
		return out, err
	}
	row, err := t.Runner.EnqueueAndExecute(withCardVaultBilling(cardguard.WithCard(ctx, req.Card), req.Billing), EnqueueParams{
		MerchantID: tid.UUID(),
		Provider:   strings.ToLower(req.Rail),
		IntentType: TypeNMICardVault,
		PspID:      req.PspID,
		Payload: NMICardVaultPayload{UserID: req.UserID, PaymentMethodID: req.PaymentMethodID,
			Card: nmiCard{LastFour: req.Card.LastFour(), CardType: req.Card.Brand(), ExpiryDate: req.Card.Expiry()}},
		IdempotencyKey: NMICardVaultIdempotencyKey(req.PaymentMethodID),
		// A worker has no card: it looks only after this request has had its turn.
		NextAttemptAt: time.Now().UTC().Add(cardVaultAttendance),
		Origin:        OriginUser,
		OriginReason:  "customer card entry",
	})
	// Whatever happened, this request is done with the card.
	req.Card.Zero()
	if err != nil {
		return out, err
	}
	if row.LastFailureReason != nil {
		out.Reason = *row.LastFailureReason
	}
	switch row.Status {
	case StatusSucceeded:
		out.VaultID, out.BillingID = CardVaultID(row.ID), CardBillingID(row.ID)
	case StatusFailedTerminal, StatusSuperseded, StatusExpired:
		if code := intentEvidenceInt(row.ResultEvidence, "response_code"); code != 0 {
			out.Refused, out.ResponseCode = true, code
		}
	default:
		out.Unknown = true
	}
	return out, nil
}

func intentEvidenceInt(raw []byte, key string) int {
	var evidence map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &evidence) != nil {
		return 0
	}
	n, _ := strconv.Atoi(string(evidence[key]))
	return n
}
