package hostedcheckout

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Store keeps checkouts on their orders.
type Store struct{ db *db.DB }

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// Attach gives an open merchant order its checkout, in the caller's
// transaction (q), and answers the checkout URL on origin. It is refused
// when the order has one already or is not an open merchant order.
func Attach(ctx context.Context, q *gen.Queries, origin string, mid billing.MerchantID, c Checkout, now time.Time) (string, error) {
	if origin == "" {
		return "", apperr.New(http.StatusBadRequest, "hosted_checkout_unavailable", "this deployment serves no hosted checkout")
	}
	secret, hash, err := NewSecret()
	if err != nil {
		return "", err
	}
	n, err := q.SetOrderCheckout(ctx, gen.SetOrderCheckoutParams{
		SecretHash: hash, ExpiresAt: c.ExpiresAt, SuccessUrl: *c.SuccessURL, CancelUrl: c.CancelURL,
		SavedPaymentMethods: c.SavedPaymentMethods, UiMode: string(c.UIMode), Now: now, MerchantID: mid.UUID(), ID: c.OrderID.UUID(),
	})
	switch {
	case err != nil:
		return "", err
	case n != 1:
		return "", apperr.New(http.StatusBadRequest, "hosted_checkout_unavailable", "only an open merchant order without a checkout takes one")
	}
	if err := q.SaveOrderCheckoutReceipt(ctx, gen.SaveOrderCheckoutReceiptParams{
		MerchantID: mid.UUID(), OrderID: c.OrderID.UUID(), Secret: secret, Now: now, ExpiresAt: c.ExpiresAt,
	}); err != nil {
		return "", err
	}
	return URL(origin, c.OrderID, secret), nil
}

// ReplayURL recovers the original capability only for an authenticated,
// matching order-creation retry. The caller has already checked the durable
// request digest and customer binding. It reads current order state rather
// than replaying a cached response, and ordinary order reads never call it.
func ReplayURL(ctx context.Context, q *gen.Queries, origin string, order gen.BillingOrder, now time.Time) (string, error) {
	if origin == "" || order.Origin != string(billing.OrderOriginMerchant) || order.Status != string(billing.OrderOpen) ||
		order.CheckoutExpiresAt == nil || !order.CheckoutExpiresAt.After(now) ||
		order.ExpiresAt != nil && !order.ExpiresAt.After(now) {
		return "", nil
	}

	row, err := q.GetIdempotencyKey(ctx, gen.GetIdempotencyKeyParams{MerchantID: order.MerchantID, Operation: "hosted_checkout_creation", IdempotencyKey: order.ID.String()})
	if db.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if row.Status != "succeeded" || !row.ExpiresAt.After(now) {
		return "", nil
	}
	var receipt struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(row.Result, &receipt); err != nil {
		return "", err
	}
	hash, ok := Hash(receipt.Secret)
	if !ok || subtle.ConstantTimeCompare(hash, order.CheckoutSecretHash) != 1 {
		return "", nil
	}
	return URL(origin, billing.OrderID(order.ID), receipt.Secret), nil
}

// Resolve finds the checkout a presented secret opens: ErrNotFound for none
// (or one belonging to a closed merchant), ErrExpired past its expiry.
func (s *Store) Resolve(ctx context.Context, secret string, now time.Time) (Checkout, error) {
	hash, ok := Hash(secret)
	if !ok || s == nil || s.db == nil {
		return Checkout{}, ErrNotFound
	}
	mids, err := s.db.GenDirectory().ResolveOrderCheckoutMerchant(ctx, hash)
	if err != nil {
		return Checkout{}, err
	}
	if len(mids) != 1 {
		return Checkout{}, ErrNotFound
	}
	mid := billing.MerchantID(mids[0])
	row, err := s.db.Gen(ctx).GetOrderCheckout(ctx, gen.GetOrderCheckoutParams{MerchantID: mid.UUID(), SecretHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkout{}, ErrNotFound
	}
	if err != nil {
		return Checkout{}, err
	}
	if row.CheckoutExpiresAt == nil || row.CheckoutSavedPaymentMethods == nil || row.CheckoutUiMode == nil {
		return Checkout{}, ErrNotFound
	}
	c := Checkout{
		MerchantID: mid, OrderID: billing.OrderID(row.ID), CustomerID: billing.CustomerID(row.CustomerID), UIMode: billing.CheckoutUIMode(*row.CheckoutUiMode),
		ExpiresAt: *row.CheckoutExpiresAt, SuccessURL: row.CheckoutSuccessUrl, CancelURL: row.CheckoutCancelUrl,
		SavedPaymentMethods: *row.CheckoutSavedPaymentMethods,
	}
	if c.Expired(now) {
		return c, ErrExpired
	}
	return c, nil
}

// MaxDeclines caps the card attempts a checkout's order may have declined
// (card testing): the merchant makes a new order after that.
const MaxDeclines = 10

// Exhausted reports whether c's order has taken MaxDeclines declines.
func (s *Store) Exhausted(ctx context.Context, c Checkout) (bool, error) {
	n, err := s.db.Gen(ctx).CountOrderDeclines(ctx, gen.CountOrderDeclinesParams{MerchantID: c.MerchantID.UUID(), OrderID: c.OrderID.UUID()})
	return n >= MaxDeclines, err
}
