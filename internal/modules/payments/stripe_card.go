package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// StripeCardDetails is the raw card payload under payment_method_details.card
// on a charge and under card on a payment_method, shared by the webhook
// handler and the reconcile/backfill.
type StripeCardDetails struct {
	Brand       string `json:"brand"`
	Last4       string `json:"last4"`
	ExpMonth    int    `json:"exp_month"`
	ExpYear     int    `json:"exp_year"`
	Fingerprint string `json:"fingerprint"`
	// Country is the issuing country, ISO 3166-1 alpha-2.
	Country string `json:"country"`
}

// NormalizeStripeCard returns nil when there is no usable card (no last4).
func NormalizeStripeCard(d StripeCardDetails) *models.Card {
	card := models.ParseCard(d.Brand, d.Last4, "")
	if card.Last4 == "" {
		return nil
	}
	if d.ExpMonth >= 1 && d.ExpMonth <= 12 && d.ExpYear >= 2000 {
		card.ExpMonth, card.ExpYear = d.ExpMonth, d.ExpYear
	}
	return &card
}

// SnapshotPaymentCard fills card_brand/card_last4 on any matching Stripe payment
// rows that don't yet have them. Idempotent and order-independent: whichever of
// charge.succeeded / invoice.paid / the backfill completes last fills the gap.
func SnapshotPaymentCard(ctx context.Context, database *db.DB, txnIDs []string, card *models.Card) error {
	if database == nil || card == nil || len(txnIDs) == 0 {
		return nil
	}
	// The merchant predicate is explicit: the payments transaction-id unique
	// indexes are partial on deleted_at, so transaction_id alone does not
	// bound this write.
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("snapshot payment card: %w", err)
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	if err := database.Gen(ctx).SnapshotPaymentCards(ctx, gen.SnapshotPaymentCardsParams{PspID: pspID,
		MerchantID:     merchantID.UUID(),
		CardBrand:      card.Brand,
		CardLast4:      card.Last4,
		TransactionIds: txnIDs,
	}); err != nil {
		return fmt.Errorf("snapshot payment card: %w", err)
	}
	return nil
}

// LinkStripeInvoicePayment records Stripe's concrete payment identifiers on an
// invoice-keyed payment row and copies an already-known card snapshot onto it.
// Stripe preview invoice.paid events can arrive with only invoice.id; the later
// invoice_payment.paid event links that invoice to the payment_intent/charge.
func LinkStripeInvoicePayment(ctx context.Context, database *db.DB, invoiceID, chargeID, paymentIntentID string) error {
	if database == nil {
		return nil
	}
	invoiceID = strings.TrimSpace(invoiceID)
	if invoiceID == "" {
		return nil
	}

	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	metadata := map[string]any{"stripe_invoice_id": invoiceID}
	if chargeID = strings.TrimSpace(chargeID); chargeID != "" {
		metadata["stripe_charge_id"] = chargeID
	}
	if paymentIntentID = strings.TrimSpace(paymentIntentID); paymentIntentID != "" {
		metadata["stripe_payment_intent_id"] = paymentIntentID
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal stripe invoice payment metadata: %w", err)
	}

	if err := database.Gen(ctx).MergeStripePaymentMetadata(ctx, gen.MergeStripePaymentMetadataParams{MerchantID: mid.UUID(), PspID: pspID,
		Patch:         encoded,
		TransactionID: invoiceID,
	}); err != nil {
		return fmt.Errorf("link stripe invoice payment metadata: %w", err)
	}

	aliases := compactStrings(chargeID, paymentIntentID)
	if len(aliases) == 0 {
		return nil
	}

	source, err := database.Gen(ctx).GetStripeAliasCardSnapshot(ctx, gen.GetStripeAliasCardSnapshotParams{MerchantID: mid.UUID(), PspID: pspID, TransactionIds: aliases})
	if db.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load stripe alias card snapshot: %w", err)
	}
	if source.CardLast4 == nil || strings.TrimSpace(*source.CardLast4) == "" {
		return nil
	}
	card := &models.Card{Last4: strings.TrimSpace(*source.CardLast4)}
	if source.CardBrand != nil {
		card.Brand = strings.TrimSpace(*source.CardBrand)
	}
	return SnapshotPaymentCard(ctx, database, []string{invoiceID}, card)
}

func compactStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

// UpsertStripeCardForCustomer mirrors one currently attached Stripe card,
// reporting whether it created the mirror. It never decides which card a
// subscription should use, and never changes a mirrored card: that is
// ObserveStripeCard's.
func UpsertStripeCardForCustomer(
	ctx context.Context,
	database *db.DB,
	customers *RailCustomerService,
	clock clockwork.Clock,
	customerID string,
	method *StripePaymentMethodState,
) (pm *models.PaymentMethod, created bool, err error) {
	customerID = strings.TrimSpace(customerID)
	if method == nil || customerID == "" || strings.TrimSpace(method.ID) == "" || database == nil || customers == nil || method.Card == nil {
		return nil, false, nil
	}
	paymentMethodID, card := strings.TrimSpace(method.ID), method.Card
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("upsert stripe payment method %s: %w", paymentMethodID, err)
	}
	userID, err := customers.GetUserIDByCustomerID(ctx, string(models.RailStripe), customerID)
	if db.IsNotFound(err) {
		// No user mapping yet (e.g. customer created out-of-band); nothing to link.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("resolve stripe customer %s: %w", customerID, err)
	}
	if strings.TrimSpace(userID) == "" {
		return nil, false, nil
	}

	now := time.Now().UTC()
	if clock != nil {
		now = clock.Now()
	}

	methods := paymentmethods.NewPaymentMethodRepo(database)
	pm, err = methods.GetByRailMethodRefForPSP(ctx, string(models.RailStripe), pspID, paymentMethodID)
	switch {
	case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
		pm = &models.PaymentMethod{
			ID:              uuidutil.NewV7(),
			CustomerID:      identity.CustomerIDFromString(userID).UUID(),
			Rail:            models.RailStripe,
			PspID:           &pspID,
			RailCustomerRef: customerID,
			RailMethodRef:   paymentMethodID,
			Fingerprint:     method.Fingerprint,
			Card:            *card,
			Status:          paymentmethods.StatusActive,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		// The customer row the method belongs to.
		if pm.CustomerID, err = db.EnsureCustomerID(ctx, database.Qx(ctx), uuid.Nil, userID); err != nil {
			return nil, false, fmt.Errorf("resolve merchant subject for stripe payment method: %w", err)
		}
		if err := methods.Create(ctx, pm); err != nil {
			return nil, false, fmt.Errorf("insert stripe payment method: %w", err)
		}
		return pm, true, nil
	case err != nil:
		return nil, false, fmt.Errorf("lookup stripe payment method: %w", err)
	}
	if pm.CustomerID != identity.CustomerIDFromString(userID).UUID() || pm.RailCustomerRef != "" && pm.RailCustomerRef != customerID {
		return nil, false, errors.New("stripe payment method belongs to a different customer")
	}
	if pm.RailCustomerRef == "" {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, false, err
		}
		rows, err := database.Gen(ctx).BindMissingStripeCustomerReference(ctx, gen.BindMissingStripeCustomerReferenceParams{MerchantID: mid.UUID(), ID: pm.ID, CustomerID: pm.CustomerID, PspID: pspID, RailMethodRef: paymentMethodID, RailCustomerRef: customerID, Now: now})
		if err != nil {
			return nil, false, err
		}
		if rows != 1 {
			return nil, false, errors.New("stripe payment method cannot adopt verified customer binding")
		}
		pm.RailCustomerRef = customerID
	}
	return pm, false, nil
}
