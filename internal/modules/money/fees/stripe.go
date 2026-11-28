// Package fees posts the processor fees a PSP charged to the general ledger.
// Stripe reports each fee on a balance transaction of the charge, refund,
// dispute or payout it belongs to; NMI and CCBill fees are left to the bank
// feed.
package fees

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const (
	// CoordSource names a fee's coordinate: its source id is the balance
	// transaction's, so each posts once whoever reads it.
	CoordSource = books.StripeBalanceTransaction
	// UnreadFindingType is the finding a read that keeps failing raises; the
	// read waits until it is resolved.
	UnreadFindingType = "books.fee.unread"

	readBatch   = 100
	unreadAfter = 8
	maxWait     = 24 * time.Hour
)

// errNotYet is an object whose balance transaction Stripe has yet to make.
var errNotYet = errors.New("stripe has no balance transaction for it yet")

// StripeReader reads the fees of the Stripe charges, refunds and disputes
// queued in stripe_fee_reads, as the billing pass's stripe_fees scan. Each fee
// posts once, at its balance transaction: DR processor_fees (chargeback_fees
// for a dispute) / CR cash_clearing of the PSP, in the transaction's own
// currency, the account's settlement currency, which may differ from the
// charge's; a negative fee posts the other way. A read that fails waits,
// doubling from a minute to a day; after eight failures it waits for its
// books.fee.unread finding to be resolved. It only ever reads (a read-only
// client): a fee never holds up the money movement it belongs to.
type StripeReader struct {
	DB     *db.DB
	Rails  railresolve.Source
	Stripe *stripeapi.Factory
	Log    *slog.Logger
}

// Pass reads every fee read due now. Each merchant's general ledger is
// opened once per pass in each currency its fees name.
func (r *StripeReader) Pass(ctx context.Context) error {
	seen, opened := map[string]bool{}, map[string]bool{}
	for {
		due, err := r.DB.GenDirectory().ListDueStripeFeeReads(ctx, readBatch)
		if err != nil {
			return err
		}
		fresh := 0
		for _, row := range due {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			key := row.MerchantID.String() + "/" + row.PspID.String() + "/" + row.Ref
			if seen[key] {
				continue
			}
			seen[key], fresh = true, fresh+1
			if err := r.read(ctx, row, opened); err != nil {
				return err
			}
		}
		if len(due) < readBatch || fresh == 0 {
			return nil
		}
	}
}

// read posts one object's fees and settles its row; only a database failure
// is the pass's.
func (r *StripeReader) read(ctx context.Context, row gen.ListDueStripeFeeReadsRow, opened map[string]bool) error {
	mid := billing.MerchantID(row.MerchantID)
	ctx = merchant.WithID(ctx, mid)
	transactions, err := r.balanceTransactions(ctx, row)
	if err == nil {
		err = r.post(ctx, row, transactions, opened)
	}
	if err == nil {
		return r.DB.RunInMerchantScope(ctx, mid, "stripe fee read", func(ctx context.Context) error {
			_, err := r.DB.Gen(ctx).DoneStripeFeeRead(ctx, gen.DoneStripeFeeReadParams{MerchantID: row.MerchantID, PspID: row.PspID, Ref: row.Ref, DueAt: row.DueAt})
			return err
		})
	}
	r.Log.WarnContext(ctx, "stripe fees: read failed; it is read again later", logx.Err(err), "merchant_id", row.MerchantID, "psp_id", row.PspID, "ref", row.Ref)
	return r.DB.RunInMerchantScope(ctx, mid, "stripe fee read", func(ctx context.Context) error {
		q := r.DB.Gen(ctx)
		reason := err.Error()
		if len(reason) > 500 {
			reason = reason[:500]
		}
		attempts, derr := q.DeferStripeFeeRead(ctx, gen.DeferStripeFeeReadParams{MerchantID: row.MerchantID, PspID: row.PspID, Ref: row.Ref, DueAt: row.DueAt,
			ParkAfter: unreadAfter, WaitSeconds: wait(row.Attempts).Seconds(), Error: reason})
		if errors.Is(derr, pgx.ErrNoRows) || derr == nil && attempts < unreadAfter {
			return nil
		}
		if derr != nil {
			return derr
		}
		return unread(ctx, q, row, reason)
	})
}

// wait is how long a read that failed attempts times waits: doubling from a
// minute, at most a day.
func wait(attempts int32) time.Duration {
	if attempts >= 11 {
		return maxWait
	}
	return min(time.Minute<<attempts, maxWait)
}

// unread raises the finding a parked read waits behind: approving it (its
// recommendation is ack_resume) reads the object again.
func unread(ctx context.Context, q *gen.Queries, row gen.ListDueStripeFeeReadsRow, reason string) error {
	evidence, err := json.Marshal(map[string]any{"psp_id": row.PspID.String(), "ref": row.Ref, "kind": row.Kind, "error": reason,
		recommend.EvidenceKey: recommend.Recommendation{Action: recommend.ActionAckResume}.Map()})
	if err != nil {
		return err
	}
	action := "OpenRails cannot read this Stripe object's balance transactions, so its processor fees are not in the general ledger. It has stopped trying: check the PSP's credentials and that the object exists, then approve this finding to read it again."
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: row.MerchantID, FindingType: UnreadFindingType, SubjectKey: row.PspID.String() + "/" + row.Ref,
		Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	return err
}

// balanceTransaction is what a fee needs of a Stripe balance transaction.
type balanceTransaction struct {
	ID       string `json:"id"`
	Fee      int64  `json:"fee"`
	Currency string `json:"currency"`
	Created  int64  `json:"created"`
}

// post posts the fees of one object's balance transactions in one
// transaction, once the merchant's general ledger is open in their
// currencies.
func (r *StripeReader) post(ctx context.Context, row gen.ListDueStripeFeeReadsRow, transactions []balanceTransaction, opened map[string]bool) error {
	var currencies []string
	for _, bt := range transactions {
		if key := row.MerchantID.String() + "/" + strings.ToUpper(bt.Currency); bt.Fee != 0 && !opened[key] {
			opened[key] = true
			currencies = append(currencies, strings.ToUpper(bt.Currency))
		}
	}
	if len(currencies) > 0 {
		books.Prepare(ctx, r.DB.DataPool(), r.Log, row.MerchantID, currencies...)
	}
	typ := books.ProcessorFee
	if row.Kind == "dispute" {
		typ = books.ChargebackFee
	}
	return r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		ledger := books.New(r.DB.NewWithPgxTx(tx).Gen(ctx), row.MerchantID)
		for _, bt := range transactions {
			if err := PostFee(ctx, ledger, row.PspID, typ, bt.ID, bt.Fee, bt.Currency, bt.Created); err != nil {
				return err
			}
		}
		return nil
	})
}

// PostFee posts one Stripe balance transaction's fee once, at the balance
// transaction: typ (processor_fee, or chargeback_fee for a dispute) DR its
// expense / CR the PSP's cash clearing, the other way round for a negative
// fee, in the transaction's currency at its creation. A zero fee posts
// nothing.
func PostFee(ctx context.Context, ledger *books.Books, psp uuid.UUID, typ books.TransferType, id string, fee int64, currency string, created int64) error {
	if fee == 0 {
		return nil
	}
	expense := books.ProcessorFees
	if typ == books.ChargebackFee {
		expense = books.ChargebackFees
	}
	currency = strings.ToUpper(currency)
	amount, err := moneyutil.RailMinorToNative(currency, moneyutil.Cents(absMinor(fee)))
	if err != nil {
		return fmt.Errorf("stripe fees: balance transaction %s: %w", id, err)
	}
	debit, credit := books.Ref{Type: expense}, books.Ref{Type: books.CashClearing, PSP: &psp}
	if fee < 0 {
		debit, credit = credit, debit
	}
	if _, _, err := ledger.Post(ctx, books.Posting{Debit: debit, Credit: credit, Amount: amount, Currency: currency, Type: typ,
		Coord:       books.Coord{Operation: books.Operation(typ), Source: CoordSource, SourceID: id},
		EffectiveAt: time.Unix(created, 0).UTC()}); err != nil {
		return fmt.Errorf("stripe fees: balance transaction %s: %w", id, err)
	}
	return nil
}

func absMinor(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	if v < 0 {
		return -v
	}
	return v
}

// balanceTransactions reads the object's balance transactions from Stripe.
func (r *StripeReader) balanceTransactions(ctx context.Context, row gen.ListDueStripeFeeReadsRow) ([]balanceTransaction, error) {
	resolved, err := r.Rails.RailConfig(ctx, "stripe", row.AccountID)
	if err != nil {
		return nil, err
	}
	if resolved.Stripe == nil || strings.TrimSpace(resolved.Stripe.SecretKey) == "" {
		return nil, errors.New("stripe fees: the PSP has no secret key")
	}
	s := stripeReads{client: r.Stripe.ReadOnlyClient(30 * time.Second), key: strings.TrimSpace(resolved.Stripe.SecretKey)}
	switch row.Kind {
	case "dispute":
		var dispute struct {
			BalanceTransactions []balanceTransaction `json:"balance_transactions"`
		}
		if err := s.get(ctx, "/v1/disputes/"+url.PathEscape(row.Ref), nil, &dispute); err != nil {
			return nil, err
		}
		return dispute.BalanceTransactions, nil
	case "refund":
		return s.single(ctx, "/v1/refunds/"+url.PathEscape(row.Ref), "balance_transaction")
	}
	ref := row.Ref
	if strings.HasPrefix(ref, "in_") {
		links, err := r.DB.Gen(merchant.WithID(ctx, billing.MerchantID(row.MerchantID))).GetStripeInvoicePaymentLinks(ctx, gen.GetStripeInvoicePaymentLinksParams{MerchantID: row.MerchantID, PspID: row.PspID, TransactionID: ref})
		if err != nil {
			return nil, fmt.Errorf("stripe fees: invoice %s: %w", ref, err)
		}
		switch {
		case links.ChargeID != "":
			ref = links.ChargeID
		case links.PaymentIntentID != "":
			ref = links.PaymentIntentID
		default:
			return nil, fmt.Errorf("stripe fees: invoice %s: no charge linked yet: %w", ref, errNotYet)
		}
	}
	switch {
	case strings.HasPrefix(ref, "pi_"):
		var intent struct {
			LatestCharge json.RawMessage `json:"latest_charge"`
		}
		if err := s.get(ctx, "/v1/payment_intents/"+url.PathEscape(ref), url.Values{"expand[]": {"latest_charge.balance_transaction"}}, &intent); err != nil {
			return nil, err
		}
		var charge struct {
			BalanceTransaction json.RawMessage `json:"balance_transaction"`
		}
		if err := json.Unmarshal(intent.LatestCharge, &charge); err != nil || len(charge.BalanceTransaction) == 0 {
			return nil, fmt.Errorf("stripe fees: payment intent %s: %w", ref, errNotYet)
		}
		return s.transaction(ctx, charge.BalanceTransaction)
	case strings.HasPrefix(ref, "ch_"), strings.HasPrefix(ref, "py_"):
		return s.single(ctx, "/v1/charges/"+url.PathEscape(ref), "balance_transaction")
	}
	return nil, fmt.Errorf("stripe fees: %s names no charge, payment intent or invoice", ref)
}

// stripeReads are one PSP's authenticated, read-only Stripe GETs.
type stripeReads struct {
	client *http.Client
	key    string
}

// single reads an object with its one balance transaction in field.
func (s stripeReads) single(ctx context.Context, path, field string) ([]balanceTransaction, error) {
	var object map[string]json.RawMessage
	if err := s.get(ctx, path, url.Values{"expand[]": {field}}, &object); err != nil {
		return nil, err
	}
	return s.transaction(ctx, object[field])
}

// transaction decodes an expanded balance transaction, or reads one Stripe
// named by id.
func (s stripeReads) transaction(ctx context.Context, raw json.RawMessage) ([]balanceTransaction, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errNotYet
	}
	var bt balanceTransaction
	if raw[0] == '"' {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return nil, err
		}
		if err := s.get(ctx, "/v1/balance_transactions/"+url.PathEscape(id), nil, &bt); err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(raw, &bt); err != nil {
		return nil, fmt.Errorf("stripe fees: parse balance transaction: %w", err)
	}
	if bt.ID == "" || bt.Currency == "" {
		return nil, errors.New("stripe fees: a balance transaction without id or currency")
	}
	return []balanceTransaction{bt}, nil
}

func (s stripeReads) get(ctx context.Context, path string, query url.Values, into any) error {
	return stripeapi.Get(ctx, s.client, s.key, path, query, into)
}
