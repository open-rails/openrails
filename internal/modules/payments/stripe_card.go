package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// StripeCardDetails is the raw card payload that appears under
// payment_method_details.card on a charge, under card on a payment_method, and
// under payment_method_details.card on a charge listed via the REST backfill.
// It lives here (not in the webhooks package) so both the webhook handler and
// the reconcile/backfill share one parse + normalize path.
type StripeCardDetails struct {
	Brand    string `json:"brand"`
	Last4    string `json:"last4"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
}

// StripeCard is a normalized card snapshot extracted from a Stripe payload.
type StripeCard struct {
	Brand  string
	Last4  string
	Expiry string // "MM/YY", empty if exp unknown
}

// NormalizeStripeCard returns nil when there is no usable card (no last4).
func NormalizeStripeCard(d StripeCardDetails) *StripeCard {
	last4 := strings.TrimSpace(d.Last4)
	if last4 == "" {
		return nil
	}
	card := &StripeCard{Brand: TitleCaseBrand(d.Brand), Last4: last4}
	if d.ExpMonth > 0 && d.ExpYear > 0 {
		card.Expiry = fmt.Sprintf("%02d/%02d", d.ExpMonth, d.ExpYear%100)
	}
	return card
}

// TitleCaseBrand title-cases the first rune of a Stripe brand string ("visa" ->
// "Visa"); it leaves the rest as-is and returns the input unchanged when empty.
func TitleCaseBrand(brand string) string {
	b := strings.TrimSpace(brand)
	if b == "" {
		return b
	}
	return strings.ToUpper(b[:1]) + b[1:]
}

// SnapshotPaymentCard fills card_brand/card_last4 on any matching Stripe payment
// rows that don't yet have them. Idempotent and order-independent: whichever of
// charge.succeeded / invoice.paid / the backfill completes last fills the gap.
func SnapshotPaymentCard(ctx context.Context, database *db.DB, txnIDs []string, card *StripeCard) error {
	if database == nil || card == nil || len(txnIDs) == 0 {
		return nil
	}
	_, err := database.GetDB().NewUpdate().
		Model((*models.Payment)(nil)).
		Set("card_brand = ?", card.Brand).
		Set("card_last4 = ?", card.Last4).
		Where("processor = ?", models.ProcessorStripe).
		Where("transaction_id IN (?)", bun.In(txnIDs)).
		Where("card_last4 IS NULL").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("snapshot payment card: %w", err)
	}
	return nil
}

// StripeCardSubscriptionLinker is the slice of the subscription service the card
// upsert needs to link the active subscription's current payment method. The
// concrete *subscriptions.SubscriptionService satisfies it; keeping it as an
// interface here avoids a payments->subscriptions import cycle.
type StripeCardSubscriptionLinker interface {
	GetActiveSubscription(ctx context.Context, userID string) (*models.Subscription, error)
	Update(ctx context.Context, subscription *models.Subscription) error
}

// UpsertStripeCardForCustomer upserts a billing.payment_methods row for the
// customer's card and links it as the active subscription's payment method, so
// the /account "current card" reads purely from the DB.
//
// It is the single shared implementation behind both the webhook handler
// (charge.succeeded / payment_method.attached) and the reconcile backfill. The
// upsert is keyed by (processor=stripe, vault_id); the subscription link is only
// written when it changes. Both make it idempotent and safe to re-run.
func UpsertStripeCardForCustomer(
	ctx context.Context,
	database *db.DB,
	customers *ProcessorCustomerService,
	subscriptions StripeCardSubscriptionLinker,
	clock clockwork.Clock,
	customerID, paymentMethodID, initialTxnID string,
	card *StripeCard,
) error {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" || database == nil || customers == nil || card == nil {
		return nil
	}
	userID, err := customers.GetUserIDByCustomerID(ctx, string(models.ProcessorStripe), customerID)
	if err != nil || strings.TrimSpace(userID) == "" {
		// No user mapping yet (e.g. customer created out-of-band); nothing to link.
		return nil
	}

	vaultID := strings.TrimSpace(paymentMethodID)
	if vaultID == "" {
		vaultID = "stripe:" + customerID
	}

	now := time.Now().UTC()
	if clock != nil {
		now = clock.Now()
	}

	bdb := database.GetDB()
	pm := new(models.PaymentMethod)
	err = bdb.NewSelect().Model(pm).
		Where("processor = ?", models.ProcessorStripe).
		Where("vault_id = ?", vaultID).
		Limit(1).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		pm = &models.PaymentMethod{
			ID:                   uuidutil.NewV7(),
			UserID:               userID,
			Processor:            models.ProcessorStripe,
			VaultID:              vaultID,
			InitialTransactionID: strings.TrimSpace(initialTxnID),
			CardType:             &card.Brand,
			LastFour:             &card.Last4,
		}
		if card.Expiry != "" {
			pm.ExpiryDate = &card.Expiry
		}
		if _, err := bdb.NewInsert().Model(pm).Exec(ctx); err != nil {
			return fmt.Errorf("insert stripe payment method: %w", err)
		}
	case err != nil:
		return fmt.Errorf("lookup stripe payment method: %w", err)
	default:
		pm.CardType = &card.Brand
		pm.LastFour = &card.Last4
		if card.Expiry != "" {
			pm.ExpiryDate = &card.Expiry
		}
		pm.UpdatedAt = now
		if _, err := bdb.NewUpdate().Model(pm).
			Column("card_type", "last_four", "expiry_date", "updated_at").
			WherePK().Exec(ctx); err != nil {
			return fmt.Errorf("update stripe payment method: %w", err)
		}
	}

	// Link as the active subscription's current payment method.
	if subscriptions == nil {
		return nil
	}
	sub, err := subscriptions.GetActiveSubscription(ctx, userID)
	if err != nil || sub == nil {
		return nil
	}
	if sub.PaymentMethodID != nil && *sub.PaymentMethodID == pm.ID {
		return nil
	}
	sub.PaymentMethodID = &pm.ID
	if err := subscriptions.Update(ctx, sub); err != nil {
		return fmt.Errorf("link subscription payment method: %w", err)
	}
	return nil
}
