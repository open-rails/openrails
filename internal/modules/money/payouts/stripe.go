// Package payouts books what a PSP pays the merchant out and what it holds
// back: each Stripe payout, from its PSP's cash clearing through cash in
// transit to the bank account it lands in, and the reserve lines paid out in
// it.
package payouts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/open-rails/openrails/internal/modules/money/fees"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// UnreadFindingType is the finding a payout read that keeps failing raises;
// the read waits until it is resolved.
const UnreadFindingType = "books.payout.unread"

const (
	readBatch   = 100
	unreadAfter = 8
	maxWait     = 24 * time.Hour
)

// StripeReader reads the Stripe payouts queued in stripe_payout_reads, as the
// billing pass's stripe_payouts scan, and books each once at its Stripe
// objects:
//
//   - sent: payout_sent DR cash_in_transit / CR cash_clearing of the PSP, at
//     the payout's balance transaction;
//   - paid: cash_deposited DR bank_cash of its bank account / CR
//     cash_in_transit, on its arrival date in the merchant's accounting zone;
//   - failed or canceled: the deposit, if it was booked, and the payout
//     posted back at the balance transaction that returned it;
//   - its reserve lines (an automatic payout's reserved_funds balance
//     transactions): reserve_held DR processor_reserve / CR cash_clearing,
//     reserve_released the other way.
//
// Each balance transaction's fee posts as a processor fee. A payout is read
// until it is paid, failed or canceled and its reserve lines are read; one
// not yet there is read again at its arrival date and each hour after. A read
// that fails waits, doubling from a minute to a day; after eight failures it
// waits for its books.payout.unread finding to be resolved. It only reads.
type StripeReader struct {
	DB     *db.DB
	Rails  railresolve.Source
	Stripe *stripeapi.Factory
	Log    *slog.Logger
}

// Pass reads every payout read due now.
func (r *StripeReader) Pass(ctx context.Context) error {
	seen := map[string]bool{}
	for {
		due, err := r.DB.GenDirectory().ListDueStripePayoutReads(ctx, readBatch)
		if err != nil {
			return err
		}
		fresh := 0
		for _, row := range due {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			key := row.MerchantID.String() + "/" + row.PspID.String() + "/" + row.PayoutID
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

// read books one payout and settles its row; only a database failure is the
// pass's.
func (r *StripeReader) read(ctx context.Context, row gen.ListDueStripePayoutReadsRow) error {
	mid := billing.MerchantID(row.MerchantID)
	ctx = merchant.WithID(ctx, mid)
	p, err := r.fetch(ctx, row)
	if err == nil {
		err = r.book(ctx, row, p)
	}
	if err == nil {
		return r.DB.RunInMerchantScope(ctx, mid, "stripe payout read", func(ctx context.Context) error {
			q := r.DB.Gen(ctx)
			if p.settled() {
				_, err := q.DoneStripePayoutRead(ctx, gen.DoneStripePayoutReadParams{MerchantID: row.MerchantID, PspID: row.PspID, PayoutID: row.PayoutID, DueAt: row.DueAt})
				return err
			}
			_, err := q.WaitStripePayoutRead(ctx, gen.WaitStripePayoutReadParams{Arrival: p.arrival(), MerchantID: row.MerchantID, PspID: row.PspID, PayoutID: row.PayoutID, DueAt: row.DueAt})
			return err
		})
	}
	r.Log.WarnContext(ctx, "stripe payouts: read failed; it is read again later", logx.Err(err), "merchant_id", row.MerchantID, "psp_id", row.PspID, "payout_id", row.PayoutID)
	return r.DB.RunInMerchantScope(ctx, mid, "stripe payout read", func(ctx context.Context) error {
		q := r.DB.Gen(ctx)
		reason := err.Error()
		if len(reason) > 500 {
			reason = reason[:500]
		}
		attempts, derr := q.DeferStripePayoutRead(ctx, gen.DeferStripePayoutReadParams{MerchantID: row.MerchantID, PspID: row.PspID, PayoutID: row.PayoutID, DueAt: row.DueAt,
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

// unread raises the finding a parked read waits behind: approving it reads
// the payout again.
func unread(ctx context.Context, q *gen.Queries, row gen.ListDueStripePayoutReadsRow, reason string) error {
	evidence, err := json.Marshal(map[string]any{"psp_id": row.PspID.String(), "payout_id": row.PayoutID, "error": reason,
		recommend.EvidenceKey: recommend.Recommendation{Action: recommend.ActionAckResume}.Map()})
	if err != nil {
		return err
	}
	action := "OpenRails cannot read this Stripe payout, so its deposit and reserve lines are not in the general ledger. It has stopped trying: check the PSP's credentials and that the payout exists, then approve this finding to read it again."
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: row.MerchantID, FindingType: UnreadFindingType, SubjectKey: row.PspID.String() + "/" + row.PayoutID,
		Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	return err
}

// balanceTransaction is what the books need of a Stripe balance transaction.
type balanceTransaction struct {
	ID          string `json:"id"`
	Amount      int64  `json:"amount"`
	Fee         int64  `json:"fee"`
	Currency    string `json:"currency"`
	Created     int64  `json:"created"`
	AvailableOn int64  `json:"available_on"`
	Type        string `json:"type"`
}

// destination is the bank account or card a payout is sent to.
type destination struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	BankName string `json:"bank_name"`
	Brand    string `json:"brand"`
	Last4    string `json:"last4"`
	Currency string `json:"currency"`
}

// payout is one Stripe payout as the books read it.
type payout struct {
	ID                   string `json:"id"`
	Amount               int64  `json:"amount"`
	Currency             string `json:"currency"`
	ArrivalDate          int64  `json:"arrival_date"`
	Automatic            bool   `json:"automatic"`
	Status               string `json:"status"`
	ReconciliationStatus string `json:"reconciliation_status"`
	PayoutMethod         string `json:"payout_method"`

	sent, returned *balanceTransaction
	dest           *destination
	reserves       []balanceTransaction
}

// final is a status a payout ends in; a paid one may still fail.
func (p *payout) final() bool {
	return p.Status == "paid" || p.Status == "failed" || p.Status == "canceled"
}

// settled is a payout read to its end: final, and its reserve lines read
// unless Stripe will never list them.
func (p *payout) settled() bool {
	return p.final() && p.ReconciliationStatus != "in_progress"
}

func (p *payout) arrival() time.Time { return time.Unix(p.ArrivalDate, 0).UTC() }

// fetch reads the payout, its balance transactions, its destination and,
// once Stripe has reconciled an automatic payout, its reserve lines.
func (r *StripeReader) fetch(ctx context.Context, row gen.ListDueStripePayoutReadsRow) (*payout, error) {
	resolved, err := r.Rails.RailConfig(ctx, "stripe", row.AccountID)
	if err != nil {
		return nil, err
	}
	if resolved.Stripe == nil || strings.TrimSpace(resolved.Stripe.SecretKey) == "" {
		return nil, errors.New("stripe payouts: the PSP has no secret key")
	}
	s := reads{client: r.Stripe.ReadOnlyClient(30 * time.Second), key: strings.TrimSpace(resolved.Stripe.SecretKey)}
	var raw struct {
		payout
		BalanceTransaction        json.RawMessage `json:"balance_transaction"`
		FailureBalanceTransaction json.RawMessage `json:"failure_balance_transaction"`
		Destination               json.RawMessage `json:"destination"`
	}
	if err := s.get(ctx, "/v1/payouts/"+url.PathEscape(row.PayoutID),
		url.Values{"expand[]": {"balance_transaction", "failure_balance_transaction", "destination"}}, &raw); err != nil {
		return nil, err
	}
	p := raw.payout
	if p.ID != row.PayoutID || p.Currency == "" {
		return nil, fmt.Errorf("stripe payouts: %s read as %q in %q", row.PayoutID, p.ID, p.Currency)
	}
	if p.sent, err = s.transaction(ctx, raw.BalanceTransaction); err != nil {
		return nil, err
	}
	if p.sent == nil || p.sent.Amount >= 0 {
		return nil, fmt.Errorf("stripe payouts: %s has no balance transaction taking it from the balance", p.ID)
	}
	if p.Status == "failed" || p.Status == "canceled" {
		if p.returned, err = s.transaction(ctx, raw.FailureBalanceTransaction); err != nil {
			return nil, err
		}
		if p.returned == nil {
			return nil, fmt.Errorf("stripe payouts: %s is %s with no balance transaction returning it", p.ID, p.Status)
		}
	}
	if p.dest, err = s.destination(ctx, raw.Destination, p.PayoutMethod); err != nil {
		return nil, err
	}
	if p.Automatic && p.ReconciliationStatus == "completed" {
		if p.reserves, err = s.reserveLines(ctx, p.ID); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// book posts what the payout shows, once each, in one transaction, once the
// merchant's general ledger is open for its PSP and bank account.
func (r *StripeReader) book(ctx context.Context, row gen.ListDueStripePayoutReadsRow, p *payout) error {
	mid := billing.MerchantID(row.MerchantID)
	var bank uuid.UUID
	var zone *time.Location
	currency := strings.ToUpper(p.Currency)
	if err := r.DB.RunInMerchantScope(ctx, mid, "stripe payout bank account", func(ctx context.Context) error {
		var err error
		if zone, err = books.AccountingZone(ctx, r.DB); err != nil {
			return err
		}
		var name *string
		if n := strings.TrimSpace(p.dest.BankName + p.dest.Brand); n != "" {
			name = &n
		}
		var last4 *string
		if l := strings.TrimSpace(p.dest.Last4); l != "" {
			last4 = &l
		}
		destCurrency := strings.ToUpper(strings.TrimSpace(p.dest.Currency))
		if destCurrency == "" {
			destCurrency = currency
		}
		bank, err = r.DB.Gen(ctx).UpsertBankAccount(ctx, gen.UpsertBankAccountParams{MerchantID: row.MerchantID, PspID: row.PspID, Ref: p.dest.ID, Name: name, Last4: last4, Currency: destCurrency})
		return err
	}); err != nil {
		return fmt.Errorf("stripe payouts: record the bank account of %s: %w", p.ID, err)
	}
	books.Prepare(ctx, r.DB.DataPool(), r.Log, row.MerchantID, currency, strings.ToUpper(p.sent.Currency))

	psp := row.PspID
	clearing := books.Ref{Type: books.CashClearing, PSP: &psp}
	transit := books.Ref{Type: books.CashInTransit, PSP: &psp}
	reserve := books.Ref{Type: books.ProcessorReserve, PSP: &psp}
	cash := books.Ref{Type: books.BankCash, Bank: &bank}
	return r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := r.DB.NewWithPgxTx(tx).Gen(ctx)
		ledger := books.New(q, row.MerchantID)
		post := func(typ books.TransferType, debit, credit books.Ref, minor int64, cur, op, source, id string, at time.Time) error {
			cur = strings.ToUpper(cur)
			amount, err := moneyutil.RailMinorToNative(cur, moneyutil.Cents(minor))
			if err != nil {
				return fmt.Errorf("stripe payouts: %s %s: %w", p.ID, id, err)
			}
			_, _, err = ledger.Post(ctx, books.Posting{Debit: debit, Credit: credit, Amount: amount, Currency: cur, Type: typ,
				Coord: books.Coord{Operation: books.Operation(op), Source: source, SourceID: id}, EffectiveAt: at})
			if err != nil {
				return fmt.Errorf("stripe payouts: %s %s: %w", p.ID, id, err)
			}
			return nil
		}
		fee := func(bt *balanceTransaction) error {
			return fees.PostFee(ctx, ledger, psp, books.ProcessorFee, bt.ID, bt.Fee, bt.Currency, bt.Created)
		}

		sent := p.sent
		if err := post(books.PayoutSent, transit, clearing, -sent.Amount, sent.Currency, string(books.PayoutSent), books.StripeBalanceTransaction, sent.ID, created(sent)); err != nil {
			return err
		}
		if err := fee(sent); err != nil {
			return err
		}
		if p.Status == "paid" {
			y, m, d := p.arrival().Date()
			if err := post(books.CashDeposited, cash, transit, p.Amount, currency, string(books.CashDeposited), books.StripePayout, p.ID, calendarutil.DayStart(y, m, d, zone)); err != nil {
				return err
			}
		}
		if ret := p.returned; ret != nil {
			op := "payout_" + p.Status
			deposited, err := depositBooked(ctx, q, row.MerchantID, currency, p.ID)
			if err != nil {
				return err
			}
			if deposited {
				if err := post(books.CashDeposited, transit, cash, p.Amount, currency, op, books.StripeBalanceTransaction, ret.ID, created(ret)); err != nil {
					return err
				}
			}
			if err := post(books.PayoutSent, clearing, transit, ret.Amount, ret.Currency, op, books.StripeBalanceTransaction, ret.ID, created(ret)); err != nil {
				return err
			}
			if err := fee(ret); err != nil {
				return err
			}
		}
		for i := range p.reserves {
			line := &p.reserves[i]
			var err error
			switch {
			case line.Amount < 0:
				err = post(books.ReserveHeld, reserve, clearing, -line.Amount, line.Currency, string(books.ReserveHeld), books.StripeBalanceTransaction, line.ID, created(line))
			case line.Amount > 0:
				err = post(books.ReserveReleased, clearing, reserve, line.Amount, line.Currency, string(books.ReserveReleased), books.StripeBalanceTransaction, line.ID, released(line))
			}
			if err != nil {
				return err
			}
			if err := fee(line); err != nil {
				return err
			}
		}
		return nil
	})
}

func created(bt *balanceTransaction) time.Time { return time.Unix(bt.Created, 0).UTC() }

// released is when a reserve came back: its balance transaction, no earlier
// than the funds were available again.
func released(bt *balanceTransaction) time.Time {
	return time.Unix(max(bt.Created, bt.AvailableOn), 0).UTC()
}

// depositBooked reports whether the payout's deposit posted.
func depositBooked(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, currency, payoutID string) (bool, error) {
	_, err := q.GetLedgerTransferAtCoordinate(ctx, gen.GetLedgerTransferAtCoordinateParams{
		MerchantID: merchantID, Book: string(books.General), Currency: currency, TransferType: string(books.CashDeposited),
		Operation: string(books.CashDeposited), Source: books.StripePayout, SourceID: payoutID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stripe payouts: read the deposit of %s: %w", payoutID, err)
	}
	return true, nil
}

// reads are one PSP's authenticated, read-only Stripe GETs.
type reads struct {
	client *http.Client
	key    string
}

func (s reads) get(ctx context.Context, path string, query url.Values, into any) error {
	return stripeapi.Get(ctx, s.client, s.key, path, query, into)
}

// transaction decodes an expanded balance transaction, or reads one Stripe
// named by id; none is nil.
func (s reads) transaction(ctx context.Context, raw json.RawMessage) (*balanceTransaction, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
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
		return nil, fmt.Errorf("stripe payouts: parse balance transaction: %w", err)
	}
	if bt.ID == "" || bt.Currency == "" {
		return nil, errors.New("stripe payouts: a balance transaction without id or currency")
	}
	return &bt, nil
}

// destination decodes an expanded destination or names an unexpanded one; a
// payout to a v2 payout method names that.
func (s reads) destination(_ context.Context, raw json.RawMessage, method string) (*destination, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	var d destination
	switch {
	case len(raw) == 0 || string(raw) == "null":
		d.ID = method
	case raw[0] == '"':
		if err := json.Unmarshal(raw, &d.ID); err != nil {
			return nil, err
		}
	default:
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("stripe payouts: parse destination: %w", err)
		}
	}
	if strings.TrimSpace(d.ID) == "" {
		return nil, errors.New("stripe payouts: a payout to no destination")
	}
	return &d, nil
}

// reserveLines lists an automatic payout's reserved_funds balance
// transactions: what Stripe held back from it and released into it.
func (s reads) reserveLines(ctx context.Context, payoutID string) ([]balanceTransaction, error) {
	var out []balanceTransaction
	query := url.Values{"payout": {payoutID}, "type": {"reserved_funds"}, "limit": {"100"}}
	for {
		var page struct {
			Data    []balanceTransaction `json:"data"`
			HasMore bool                 `json:"has_more"`
		}
		if err := s.get(ctx, "/v1/balance_transactions", query, &page); err != nil {
			return nil, err
		}
		for _, bt := range page.Data {
			if bt.ID == "" || bt.Currency == "" || bt.Type != "reserved_funds" {
				return nil, fmt.Errorf("stripe payouts: %s lists %q as a reserve line", payoutID, bt.ID)
			}
		}
		out = append(out, page.Data...)
		if !page.HasMore || len(page.Data) == 0 {
			return out, nil
		}
		query.Set("starting_after", page.Data[len(page.Data)-1].ID)
	}
}
