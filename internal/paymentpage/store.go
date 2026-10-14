package paymentpage

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

// Store keeps pay URLs on their orders.
type Store struct{ db *db.DB }

func NewStore(d *db.DB) *Store { return &Store{db: d} }

// Attach gives an open merchant order its pay URL, in the caller's
// transaction (q), and answers the pay URL on origin. It is refused
// when the order has one already or is not an open merchant order.
func Attach(ctx context.Context, q *gen.Queries, origin string, mid billing.MerchantID, c PayURL, now time.Time) (string, error) {
	if origin == "" {
		return "", apperr.New(http.StatusBadRequest, "pay_url_unavailable", "this deployment serves no checkout origin")
	}
	secret, hash, err := NewSecret()
	if err != nil {
		return "", err
	}
	n, err := q.SetOrderPayURL(ctx, gen.SetOrderPayURLParams{
		SecretHash: hash, ExpiresAt: c.ExpiresAt, SuccessUrl: *c.SuccessURL, CancelUrl: c.CancelURL,
		SavedPaymentMethods: c.SavedPaymentMethods, Embedded: c.Embedded, Now: now, MerchantID: mid.UUID(), ID: c.OrderID.UUID(),
	})
	switch {
	case err != nil:
		return "", err
	case n != 1:
		return "", apperr.New(http.StatusBadRequest, "pay_url_unavailable", "only an open merchant order without a pay URL takes one")
	}
	if err := q.SaveOrderPayURLReceipt(ctx, gen.SaveOrderPayURLReceiptParams{
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
		order.PayUrlExpiresAt == nil || !order.PayUrlExpiresAt.After(now) ||
		order.ExpiresAt != nil && !order.ExpiresAt.After(now) {
		return "", nil
	}

	row, err := q.GetIdempotencyKey(ctx, gen.GetIdempotencyKeyParams{MerchantID: order.MerchantID, Operation: "pay_url_creation", IdempotencyKey: order.ID.String()})
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
	if !ok || subtle.ConstantTimeCompare(hash, order.PayUrlSecretHash) != 1 {
		return "", nil
	}
	return URL(origin, billing.OrderID(order.ID), receipt.Secret), nil
}

// Resolve finds the pay URL a presented secret opens: ErrNotFound for none
// (or one belonging to a closed merchant), ErrExpired past its expiry.
func (s *Store) Resolve(ctx context.Context, secret string, now time.Time) (PayURL, error) {
	hash, ok := Hash(secret)
	if !ok || s == nil || s.db == nil {
		return PayURL{}, ErrNotFound
	}
	mids, err := s.db.GenDirectory().ResolveOrderPayURLMerchant(ctx, hash)
	if err != nil {
		return PayURL{}, err
	}
	if len(mids) != 1 {
		return PayURL{}, ErrNotFound
	}
	mid := billing.MerchantID(mids[0])
	row, err := s.db.Gen(ctx).GetOrderPayURL(ctx, gen.GetOrderPayURLParams{MerchantID: mid.UUID(), SecretHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return PayURL{}, ErrNotFound
	}
	if err != nil {
		return PayURL{}, err
	}
	if row.PayUrlExpiresAt == nil || row.SavedPaymentMethods == nil || row.Embedded == nil {
		return PayURL{}, ErrNotFound
	}
	c := PayURL{
		MerchantID: mid, OrderID: billing.OrderID(row.ID), CustomerID: billing.CustomerID(row.CustomerID), Embedded: *row.Embedded,
		ExpiresAt: *row.PayUrlExpiresAt, SuccessURL: row.SuccessUrl, CancelURL: row.CancelUrl,
		SavedPaymentMethods: *row.SavedPaymentMethods,
	}
	if c.Expired(now) {
		return c, ErrExpired
	}
	return c, nil
}

// MaxDeclines caps the card attempts a pay URL's order may have declined
// (card testing): the merchant makes a new order after that.
const MaxDeclines = 10

// Exhausted reports whether c's order has taken MaxDeclines declines.
func (s *Store) Exhausted(ctx context.Context, c PayURL) (bool, error) {
	n, err := s.db.Gen(ctx).CountOrderDeclines(ctx, gen.CountOrderDeclinesParams{MerchantID: c.MerchantID.UUID(), OrderID: c.OrderID.UUID()})
	return n >= MaxDeclines, err
}
