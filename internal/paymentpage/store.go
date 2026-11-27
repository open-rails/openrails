package paymentpage

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// Store keeps pay URLs on their invoices.
type Store struct {
	db  *db.DB
	Log *slog.Logger
}

func NewStore(d *db.DB) *Store { return &Store{db: d, Log: d.Log} }

// Attach gives an open merchant invoice its pay URL, in the caller's
// transaction (q), and answers the pay URL on origin. It is refused
// when the invoice has one already or is not an open merchant invoice.
func Attach(ctx context.Context, q *gen.Queries, origin string, mid billing.MerchantID, c PayURL, now time.Time) (string, error) {
	if origin == "" {
		return "", apperr.New(http.StatusBadRequest, billing.CodePayURLUnavailable, "this deployment serves no checkout origin")
	}
	secret, hash, err := NewSecret()
	if err != nil {
		return "", err
	}
	n, err := q.SetInvoicePayURL(ctx, gen.SetInvoicePayURLParams{
		SecretHash: hash, ExpiresAt: c.ExpiresAt, SuccessUrl: *c.SuccessURL, CancelUrl: c.CancelURL,
		SavedPaymentMethods: c.SavedPaymentMethods, Embedded: c.Embedded, Now: now, MerchantID: mid.UUID(), ID: c.InvoiceID.UUID(),
	})
	switch {
	case err != nil:
		return "", err
	case n != 1:
		return "", apperr.New(http.StatusBadRequest, billing.CodePayURLUnavailable, "only an open merchant invoice without a pay URL takes one")
	}
	if err := q.SaveInvoicePayURLReceipt(ctx, gen.SaveInvoicePayURLReceiptParams{
		MerchantID: mid.UUID(), InvoiceID: c.InvoiceID.UUID(), Secret: secret, Now: now, ExpiresAt: c.ExpiresAt,
	}); err != nil {
		return "", err
	}
	return URL(origin, c.InvoiceID, secret), nil
}

// ReplayURL recovers the original capability only for an authenticated,
// matching invoice-creation retry. The caller has already checked the durable
// request digest and customer binding. It reads current invoice state rather
// than replaying a cached response, and ordinary invoice reads never call it.
func ReplayURL(ctx context.Context, q *gen.Queries, origin string, invoice gen.BillingInvoice, now time.Time) (string, error) {
	if origin == "" || invoice.Origin != string(billing.InvoiceOriginMerchant) || invoice.Status != string(billing.InvoiceOpen) ||
		invoice.PayUrlExpiresAt == nil || !invoice.PayUrlExpiresAt.After(now) {
		return "", nil
	}

	row, err := q.GetIdempotencyKey(ctx, gen.GetIdempotencyKeyParams{MerchantID: invoice.MerchantID, Operation: "pay_url_creation", IdempotencyKey: invoice.ID.String()})
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
	if !ok || subtle.ConstantTimeCompare(hash, invoice.PayUrlSecretHash) != 1 {
		return "", nil
	}
	return URL(origin, billing.InvoiceID(invoice.ID), receipt.Secret), nil
}

// Resolve finds the pay URL a presented secret opens: ErrNotFound for none
// (or one belonging to a closed merchant), ErrExpired past its expiry.
func (s *Store) Resolve(ctx context.Context, secret string, now time.Time) (PayURL, error) {
	hash, ok := Hash(secret)
	if !ok || s == nil || s.db == nil {
		return PayURL{}, ErrNotFound
	}
	mids, err := s.db.GenDirectory().ResolveInvoicePayURLMerchant(ctx, hash)
	if err != nil {
		return PayURL{}, err
	}
	if len(mids) != 1 {
		return PayURL{}, ErrNotFound
	}
	mid := billing.MerchantID(mids[0])
	row, err := s.db.Gen(ctx).GetInvoicePayURL(ctx, gen.GetInvoicePayURLParams{MerchantID: mid.UUID(), SecretHash: hash})
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
		MerchantID: mid, InvoiceID: billing.InvoiceID(row.ID), CustomerID: billing.CustomerID(row.CustomerID), Embedded: *row.Embedded,
		ExpiresAt: *row.PayUrlExpiresAt, SuccessURL: row.SuccessUrl, CancelURL: row.CancelUrl,
		SavedPaymentMethods: *row.SavedPaymentMethods,
	}
	if c.Expired(now) {
		return c, ErrExpired
	}
	return c, nil
}

// MaxDeclines caps the card attempts a pay URL's invoice may have declined
// (card testing): the merchant makes a new invoice after that.
const MaxDeclines = 10

// Exhausted reports whether c's invoice has taken MaxDeclines declines.
func (s *Store) Exhausted(ctx context.Context, c PayURL) (bool, error) {
	n, err := s.db.Gen(ctx).CountInvoiceDeclines(ctx, gen.CountInvoiceDeclinesParams{MerchantID: c.MerchantID.UUID(), InvoiceID: c.InvoiceID.UUID()})
	return n >= MaxDeclines, err
}
