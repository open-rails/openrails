// Package fees posts the processor fees a PSP charged to the general ledger.
// Stripe reports each fee on a balance transaction of the charge, refund or
// dispute it belongs to; NMI and CCBill fees are left to the bank feed.
package fees

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const (
	// CoordSource names a fee's coordinate: its source id is the balance
	// transaction's, so each posts once whoever reads it.
	CoordSource = "stripe_balance_transaction"
	// UnreadFindingType is the finding a read that keeps failing raises.
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
// charge's; a negative fee posts the other way. A read that fails waits and
// is read again, and only ever reads (a read-only client): a fee never holds
// up the money movement it belongs to.
type StripeReader struct {
	DB     *db.DB
	Rails  railresolve.Source
	Stripe *stripeapi.Factory
	Log    *slog.Logger
}

// Pass reads every fee read due now.
func (r *StripeReader) Pass(ctx context.Context) error {
	seen := map[string]bool{}
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
			if err := r.read(ctx, row); err != nil {
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
func (r *StripeReader) read(ctx context.Context, row gen.ListDueStripeFeeReadsRow) error {
	mid := billing.MerchantID(row.MerchantID)
	ctx = merchant.WithID(ctx, mid)
	transactions, err := r.balanceTransactions(ctx, row)
	if err == nil {
		err = r.post(ctx, row, transactions)
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
			WaitSeconds: wait(row.Attempts).Seconds(), Error: reason})
		if errors.Is(derr, pgx.ErrNoRows) || derr == nil && attempts != unreadAfter {
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

func unread(ctx context.Context, q *gen.Queries, row gen.ListDueStripeFeeReadsRow, reason string) error {
	evidence, err := json.Marshal(map[string]string{"psp_id": row.PspID.String(), "ref": row.Ref, "kind": row.Kind, "error": reason})
	if err != nil {
		return err
	}
	action := "OpenRails cannot read this Stripe object's balance transactions, so its processor fees are not in the general ledger. It keeps trying daily; check the PSP's credentials and that the object exists."
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
func (r *StripeReader) post(ctx context.Context, row gen.ListDueStripeFeeReadsRow, transactions []balanceTransaction) error {
	var currencies []string
	for _, bt := range transactions {
		currencies = append(currencies, strings.ToUpper(bt.Currency))
	}
	books.Prepare(ctx, r.DB.DataPool(), r.Log, row.MerchantID, currencies...)
	typ, expense := books.ProcessorFee, books.ProcessorFees
	if row.Kind == "dispute" {
		typ, expense = books.ChargebackFee, books.ChargebackFees
	}
	psp := row.PspID
	return r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		ledger := books.New(r.DB.NewWithPgxTx(tx).Gen(ctx), row.MerchantID)
		for _, bt := range transactions {
			if bt.Fee == 0 {
				continue
			}
			currency := strings.ToUpper(bt.Currency)
			amount, err := moneyutil.RailMinorToNative(currency, moneyutil.Cents(absMinor(bt.Fee)))
			if err != nil {
				return fmt.Errorf("stripe fees: balance transaction %s: %w", bt.ID, err)
			}
			debit, credit := books.Ref{Type: expense}, books.Ref{Type: books.CashClearing, PSP: &psp}
			if bt.Fee < 0 {
				debit, credit = credit, debit
			}
			if _, _, err := ledger.Post(ctx, books.Posting{Debit: debit, Credit: credit, Amount: amount, Currency: currency, Type: typ,
				Coord:       books.Coord{Operation: books.Operation(typ), Source: CoordSource, SourceID: bt.ID},
				EffectiveAt: time.Unix(bt.Created, 0).UTC()}); err != nil {
				return fmt.Errorf("stripe fees: balance transaction %s: %w", bt.ID, err)
			}
		}
		return nil
	})
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
	target := stripeapi.APIBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stripe fees: GET %s answered %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("stripe fees: parse %s: %w", path, err)
	}
	return nil
}
