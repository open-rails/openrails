package subscriptions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
)

// SubscriptionCollectionOperationID identifies one newly accepted renewal
// attempt across copies of the same billing book. Transport retries keep it;
// only a conclusively released attempt allows the next ordinal. Caller keys,
// admission time, card changes and credential rotation never select a new ID.
func SubscriptionCollectionOperationID(merchantID, pspID uuid.UUID, p SubscriptionCollectionPayload) uuid.UUID {
	return renewalOperationID(merchantID, pspID, ObligationOrderReference(p.Renewal.SubscriptionID, p.PreviousPeriodEnd), p.Attempt)
}

func renewalOperationID(merchantID, pspID uuid.UUID, obligation string, attempt int) uuid.UUID {
	key := fmt.Sprintf("openrails:subscription-collection:v1:%s:%s:%s:%d", merchantID, pspID, obligation, attempt)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key))
}

// StripeRenewal binds the provider object to the obligation, its attempt and
// the accepted economic terms. It is absent on already accepted older operations
// whose provider identity and metadata must not change during recovery.
type StripeRenewal struct {
	Obligation  string
	Attempt     int
	TermsSHA256 string
}

// NewStripeRenewal binds provider metadata to accepted payment/access facts.
func NewStripeRenewal(p SubscriptionCollectionPayload) (StripeRenewal, error) {
	// These are payment/access facts, not a hash of the operation's serialized
	// payload. Ordinary admission timestamps, request keys, display labels and
	// local reprice bookkeeping do not change a provider request.
	// Field names and normalization persist at the provider: harmless Go
	// refactors must preserve this representation and already accepted bindings.
	entitlements := models.CloneEntitlementsSpec(p.Renewal.Entitlements)
	if entitlements == nil {
		entitlements = map[string]*int{}
	}
	terms := struct {
		PriceID      uuid.UUID       `json:"price_id"`
		ProductID    uuid.UUID       `json:"product_id"`
		Amount       int64           `json:"amount,string"`
		Currency     string          `json:"currency"`
		StartsAt     string          `json:"starts_at"`
		EndsAt       string          `json:"ends_at"`
		Entitlements map[string]*int `json:"entitlements"`
	}{p.Renewal.PriceID, p.Renewal.ProductID, p.Renewal.Amount, p.Renewal.Currency,
		p.Renewal.PeriodStart.UTC().Format(time.RFC3339Nano), p.Renewal.PeriodEnd.UTC().Format(time.RFC3339Nano), entitlements}
	raw, err := json.Marshal(terms)
	if err != nil {
		return StripeRenewal{}, err
	}
	digest := sha256.Sum256(raw)
	return StripeRenewal{Obligation: p.OrderReference, Attempt: p.Attempt, TermsSHA256: hex.EncodeToString(digest[:])}, nil
}
