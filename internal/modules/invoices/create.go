package invoices

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/invoices/invoiceevent"
	"github.com/open-rails/openrails/internal/notificationinbox"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Preview prices lines for customer without writing.
func (s *Service) Preview(ctx context.Context, customerID uuid.UUID, lines []billing.InvoiceLineParams) (*Quote, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.quote(ctx, s.DB.Gen(ctx), mid.UUID(), customerID, lines, uuid.Nil, s.now())
}

// PreviewToReplace prices the customer's own lines as FindOrCreate buys
// them: the first refused line, when held by their open invoice that
// FindOrCreate would cancel for this one, is priced as if that invoice were
// gone.
func (s *Service) PreviewToReplace(ctx context.Context, customerID uuid.UUID, lines []billing.InvoiceLineParams) (*Quote, error) {
	quote, err := s.Preview(ctx, customerID, lines)
	if err != nil {
		return nil, err
	}
	i := quote.Refused()
	if i < 0 {
		return quote, nil
	}
	held, ok := (&LineError{Index: i, Refusal: *quote.Lines[i].Refusal}).HeldBy()
	if !ok {
		return quote, nil
	}
	replaceable, err := s.replaceable(ctx, customerID, held)
	if err != nil {
		return nil, err
	}
	if !replaceable {
		return quote, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.quote(ctx, s.DB.Gen(ctx), mid.UUID(), customerID, lines, held, s.now())
}

// CreateInput is a new invoice. IdempotencyKey is the request's scoped key,
// kept on the invoice with the request's digest.
type CreateInput struct {
	CustomerID     uuid.UUID
	Origin         billing.InvoiceOrigin
	Lines          []billing.InvoiceLineParams
	ExpectedTotal  *int64
	IdempotencyKey string
	RequestDigest  []byte
	TTL            time.Duration
	// FromCart buys the customer's cart instead of Lines, emptying it in the
	// invoice's transaction.
	FromCart bool
	// Attach runs in the invoice's transaction once the invoice exists, before a
	// zero total completes it: the payment page is written with it.
	Attach func(ctx context.Context, q *gen.Queries, invoice uuid.UUID) error
	// LinesDigest names a customer invoice's lines (LinesDigest): FindOrCreate
	// answers the customer's open invoice of the same lines instead.
	LinesDigest []byte
	// Checkout is what the creating request tells of the customer; an open
	// invoice found instead keeps its own.
	Checkout CheckoutFacts
	// BalanceKey is a customer request's scoped Idempotency-Key and
	// BalanceDigest its digest: an invoice in a merchant's unit, paid as it is
	// created, keeps them, so a retry answers it.
	BalanceKey    string
	BalanceDigest []byte
}

var (
	errUnitInvoiceKey       = apperr.New(http.StatusBadRequest, billing.CodeIdempotencyKeyRequired, "An invoice in a merchant's unit is paid from the balance as it is created: the Idempotency-Key header is required.")
	errUnitInvoiceKeyReused = apperr.New(http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused, "The Idempotency-Key named another invoice in a merchant's unit.")
)

// LineKey is one line as LinesDigest names it: its price (by storage id),
// quantity and customer-chosen amount.
type LineKey struct {
	Price    uuid.UUID
	Quantity *int
	Amount   *int64
}

// LinesDigest names a set of lines, in any order.
func LinesDigest(lines []LineKey) []byte {
	keys := make([]string, len(lines))
	for i, l := range lines {
		quantity, amount := "-", "-"
		if l.Quantity != nil {
			quantity = strconv.Itoa(*l.Quantity)
		}
		if l.Amount != nil {
			amount = strconv.FormatInt(*l.Amount, 10)
		}
		keys[i] = l.Price.String() + "|" + quantity + "|" + amount
	}
	slices.Sort(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return sum[:]
}

// staleOpenInvoice is an open invoice past its expiry, of the lines or holding a
// claim they take, which a create expires before it tries again.
type staleOpenInvoice struct{ id uuid.UUID }

func (e *staleOpenInvoice) Error() string { return "invoices: the open invoice of these lines expired" }

// FindOrCreate answers the customer's open, unexpired invoice of
// in.LinesDigest's lines (found), else creates it as Create does. One
// customer's invoices are made one at a time, so two requests make one invoice.
// The customer's open invoice of other lines that holds what these buy (another
// price of the product, left unpaid after a decline) is canceled for this
// one, so the customer pays the price they chose; an invoice whose payment is
// in flight keeps the claim and the lines are refused.
func (s *Service) FindOrCreate(ctx context.Context, in CreateInput) (*Invoice, bool, error) {
	if len(in.LinesDigest) == 0 {
		return nil, false, errors.New("invoices: find-or-create needs the lines' digest")
	}
	for range 3 {
		invoice, found, err := s.create(ctx, in)
		var stale *staleOpenInvoice
		var refused *LineError
		switch {
		case errors.As(err, &stale):
			err = s.close(ctx, in.CustomerID, stale.id, billing.InvoiceExpired)
		case errors.As(err, &refused):
			held, ok := refused.HeldBy()
			if !ok {
				return nil, false, err
			}
			replaced, rerr := s.replace(ctx, in.CustomerID, held)
			if rerr != nil {
				return nil, false, rerr
			}
			if !replaced {
				return nil, false, err
			}
			err = nil
		default:
			return invoice, found, err
		}
		if err != nil {
			return nil, false, err
		}
	}
	return nil, false, errOpenInvoiceClosing
}

// replace cancels the customer's own open invoice held when it is replaceable.
func (s *Service) replace(ctx context.Context, customerID, held uuid.UUID) (bool, error) {
	replaceable, err := s.replaceable(ctx, customerID, held)
	if err != nil || !replaceable {
		return false, err
	}
	err = s.close(ctx, customerID, held, billing.InvoiceCanceled)
	if errors.Is(err, ErrNotCancelable) || errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// replaceable reports whether held is the customer's own invoice that buys
// only what is being chosen again and is still open for OpenRails to cancel:
// not another origin's, a cart's other lines, or one whose payment started.
// close rechecks it under the invoice's lock.
func (s *Service) replaceable(ctx context.Context, customerID, held uuid.UUID) (bool, error) {
	invoice, err := s.GetForCustomer(ctx, customerID, held)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return invoice.Origin == string(billing.InvoiceOriginCustomer) && len(invoice.Lines) == 1 &&
		invoice.CollectionOwner == string(billing.InvoiceCollectionEngine) && invoice.Status == string(billing.InvoiceOpen), nil
}

var errOpenInvoiceClosing = apperr.New(http.StatusConflict, billing.CodeResourceConflict, "The customer's open invoice of these lines is closing; try again.")

// Create freezes lines into an invoice and claims what its unique lines buy.
// A refused line is a LineError; a total other than ExpectedTotal is
// ErrTotalChanged. An invoice with nothing to pay is paid at once.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Invoice, error) {
	for range 3 {
		invoice, _, err := s.create(ctx, in)
		var stale *staleOpenInvoice
		if !errors.As(err, &stale) {
			return invoice, err
		}
		if err := s.close(ctx, in.CustomerID, stale.id, billing.InvoiceExpired); err != nil {
			return nil, err
		}
	}
	return nil, errOpenInvoiceClosing
}

func (s *Service) create(ctx context.Context, in CreateInput) (*Invoice, bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	if in.CustomerID == uuid.Nil || in.TTL <= 0 || (in.IdempotencyKey == "") != (len(in.RequestDigest) == 0) {
		return nil, false, errors.New("invoice needs a customer, a lifetime and a keyed digest")
	}
	id := uuidutil.NewV7()
	found := false
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if err := db.EnsureCustomerRowQ(ctx, q, mid.UUID(), in.CustomerID); err != nil {
			return err
		}
		// One customer's purchases settle one at a time.
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: in.CustomerID}); err != nil {
			return err
		}
		// A retry of an invoice in a merchant's unit, which the balance paid as
		// it was created, answers that invoice before anything else is read.
		if in.BalanceKey != "" && in.IdempotencyKey == "" {
			prior, err := q.GetInvoiceByIdempotencyKey(ctx, gen.GetInvoiceByIdempotencyKeyParams{MerchantID: mid.UUID(), IdempotencyKey: in.BalanceKey})
			switch {
			case err == nil && (prior.CustomerID != in.CustomerID || !bytes.Equal(prior.RequestDigest, in.BalanceDigest)):
				return errUnitInvoiceKeyReused
			case err == nil:
				id, found = prior.ID, true
				return nil
			case !db.IsNotFound(err):
				return err
			}
		}
		if len(in.LinesDigest) > 0 {
			open, err := q.GetOpenInvoiceByLines(ctx, gen.GetOpenInvoiceByLinesParams{MerchantID: mid.UUID(), CustomerID: in.CustomerID, LinesDigest: in.LinesDigest})
			switch {
			case err == nil && open.ExpiresAt != nil && !open.ExpiresAt.After(s.now()):
				return &staleOpenInvoice{id: open.ID}
			case err == nil:
				// Answering it is ordering again: the customer still holds what
				// its products require.
				lines, err := q.ListInvoiceLines(ctx, gen.ListInvoiceLinesParams{MerchantID: mid.UUID(), InvoiceIds: []uuid.UUID{open.ID}})
				if err != nil {
					return err
				}
				if err := entitled(ctx, q, mid.UUID(), in.CustomerID, string(in.Origin), lines, s.now()); err != nil {
					return err
				}
				id, found = open.ID, true
				return nil
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
		}
		lines := in.Lines
		if in.FromCart {
			taken, err := s.takeCart(ctx, q, mid.UUID(), in.CustomerID)
			if err != nil {
				return err
			}
			lines = taken
		}
		now := s.now()
		quote, err := s.quote(ctx, q, mid.UUID(), in.CustomerID, lines, uuid.Nil, now)
		if err != nil {
			return err
		}
		if i := quote.Refused(); i >= 0 {
			return &LineError{Index: i, Refusal: *quote.Lines[i].Refusal}
		}
		if in.ExpectedTotal != nil && *in.ExpectedTotal != quote.Total {
			return ErrTotalChanged
		}
		unit := InMerchantUnit(quote.Currency)
		keyed, digest := in.IdempotencyKey, in.RequestDigest
		// An invoice in a merchant's unit is paid as it is created: it keeps
		// its key, which answers a retry instead of spending again.
		if unit && quote.Total > 0 && keyed == "" {
			if in.BalanceKey == "" {
				return errUnitInvoiceKey
			}
			keyed, digest = in.BalanceKey, in.BalanceDigest
		}
		var key *string
		if keyed != "" {
			key = &keyed
		}
		params := gen.CreateInvoiceParams{MerchantID: mid.UUID(), ID: id, CustomerID: in.CustomerID, Origin: string(in.Origin),
			Currency: quote.Currency, Total: quote.Total, IdempotencyKey: key, RequestDigest: digest, LinesDigest: in.LinesDigest,
			ExpiresAt: now.Add(in.TTL), Now: now}
		// Spending a merchant's units is not a checkout: no facts.
		if !unit {
			if err := checkoutFacts(ctx, q, &params, in.Checkout); err != nil {
				return err
			}
		}
		if err := q.CreateInvoice(ctx, params); err != nil {
			return err
		}
		for i, l := range quote.Lines {
			credit, err := json.Marshal(l.Credit)
			if err != nil {
				return err
			}
			if l.Credit == nil {
				credit = nil
			}
			var claim *string
			if l.ClaimKey != "" {
				claim = &l.ClaimKey
			}
			quantity, err := int32Ptr(l.Quantity)
			if err != nil {
				return err
			}
			interval, err := int32Ptr(l.Price.RecurringCycleHours())
			if err != nil {
				return err
			}
			months, err := int32Ptr(l.Price.BillingIntervalMonths)
			if err != nil {
				return err
			}
			access, err := int32Ptr(l.Price.AccessDurationHours)
			if err != nil {
				return err
			}
			if err := q.CreateInvoiceLine(ctx, gen.CreateInvoiceLineParams{MerchantID: mid.UUID(), ID: uuidutil.NewV7(), InvoiceID: id, CustomerID: in.CustomerID,
				Position: int32(i), PriceID: l.Price.ID, ProductID: l.Product.ID, Description: l.Product.DisplayName, Quantity: quantity,
				UnitAmount: l.UnitAmount, Amount: l.Amount, Ownership: string(l.Ownership), ClaimKey: claim,
				BillingIntervalHours: interval, BillingIntervalMonths: months, AccessDurationHours: access, ProviderAccessDefault: l.Price.ProviderAccessDefault,
				CreditGrant: credit, Now: now}); err != nil {
				return err
			}
			if l.ClaimKey == "" {
				continue
			}
			n, err := q.ClaimOwnership(ctx, gen.ClaimOwnershipParams{MerchantID: mid.UUID(), CustomerID: in.CustomerID, ClaimKey: l.ClaimKey, InvoiceID: id, Now: now})
			if err != nil {
				return err
			}
			if n == 0 {
				held, err := q.GetOwnershipClaim(ctx, gen.GetOwnershipClaimParams{MerchantID: mid.UUID(), CustomerID: in.CustomerID, ClaimKey: l.ClaimKey})
				if err != nil {
					return err
				}
				gone, err := lapsedHolders(ctx, q, mid.UUID(), []gen.BillingOwnershipClaim{held}, now)
				if err != nil {
					return err
				}
				if gone[held.InvoiceID] {
					return &staleOpenInvoice{id: held.InvoiceID}
				}
				return &LineError{Index: i, Refusal: *claimRefusal(held)}
			}
		}
		if in.Attach != nil {
			if unit {
				return apperr.Invalidf("an invoice in a merchant's unit is paid from the customer's balance as it is created: it has no pay URL").WithParam("pay_url")
			}
			if err := in.Attach(ctx, q, id); err != nil {
				return err
			}
		}
		if quote.Total > 0 && !unit {
			return nil
		}
		invoice, err := s.load(ctx, q, mid.UUID(), id, true)
		if err != nil {
			return err
		}
		if quote.Total > 0 {
			_, err = s.payFromBalance(ctx, d, invoice)
			return err
		}
		_, err = s.settle(ctx, d, invoice, nil)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	invoice, err := s.Get(ctx, id)
	return invoice, found, err
}

// Get reads an invoice of the context's merchant.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Invoice, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.DB.Gen(ctx), mid.UUID(), id, false)
}

// GetForCustomer reads customer's own invoice; another's does not exist.
func (s *Service) GetForCustomer(ctx context.Context, customerID, id uuid.UUID) (*Invoice, error) {
	invoice, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if invoice.CustomerID != customerID {
		return nil, ErrNotFound
	}
	return invoice, nil
}

// ByIdempotencyKey is the invoice a scoped create key made, for any customer;
// nil when none.
func (s *Service) ByIdempotencyKey(ctx context.Context, key string) (*Invoice, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.DB.Gen(ctx).GetInvoiceByIdempotencyKey(ctx, gen.GetInvoiceByIdempotencyKeyParams{MerchantID: mid.UUID(), IdempotencyKey: key})
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.withLines(ctx, s.DB.Gen(ctx), mid.UUID(), row)
}

// ListFilter narrows a page of invoices; a nil field does not filter.
type ListFilter struct {
	CustomerID     *uuid.UUID
	PriceID        *uuid.UUID
	SubscriptionID *uuid.UUID
	Status         *string
	Origin         *string
	// OverdueAt lists the open invoices due before it.
	OverdueAt *time.Time
}

// List pages the merchant's invoices, newest first.
func (s *Service) List(ctx context.Context, filter ListFilter, page billing.PageRequest) (billing.ListPage[*Invoice], error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[*Invoice]{}, err
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[*Invoice]{}, err
	}
	at, after, err := pagination.After(page.Cursor)
	if err != nil {
		return billing.ListPage[*Invoice]{}, err
	}
	q := s.DB.Gen(ctx)
	rows, err := q.ListInvoicesPage(ctx, gen.ListInvoicesPageParams{MerchantID: mid.UUID(), CustomerID: filter.CustomerID, PriceID: filter.PriceID, Status: filter.Status, Now: s.now(),
		Origin: filter.Origin, SubscriptionID: filter.SubscriptionID, OverdueAt: filter.OverdueAt, AfterCreatedAt: at, AfterID: after, RowLimit: pagination.Fetch(limit)})
	if err != nil {
		return billing.ListPage[*Invoice]{}, err
	}
	cut := pagination.Cut(rows, limit, func(r gen.BillingInvoice) any { return pagination.TimeID{At: r.CreatedAt, ID: r.ID} })
	items, err := s.attachLines(ctx, q, mid.UUID(), cut.Items)
	if err != nil {
		return billing.ListPage[*Invoice]{}, err
	}
	return billing.ListPage[*Invoice]{Items: items, Next: cut.Next}, nil
}

// ListByIDs reads the merchant's named invoices, newest first; unknown ones
// are absent.
func (s *Service) ListByIDs(ctx context.Context, ids []uuid.UUID) ([]*Invoice, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Gen(ctx)
	rows, err := q.ListInvoicesByIDs(ctx, gen.ListInvoicesByIDsParams{MerchantID: mid.UUID(), Ids: ids})
	if err != nil {
		return nil, err
	}
	return s.attachLines(ctx, q, mid.UUID(), rows)
}

// attachLines reads the lines of a page of invoices.
func (s *Service) attachLines(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, rows []gen.BillingInvoice) ([]*Invoice, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	lines, err := q.ListInvoiceLines(ctx, gen.ListInvoiceLinesParams{MerchantID: merchantID, InvoiceIds: ids})
	if err != nil {
		return nil, err
	}
	refs, err := invoicePriceReferences(ctx, q, lines)
	if err != nil {
		return nil, err
	}
	byInvoice := map[uuid.UUID][]gen.BillingInvoiceLine{}
	for _, l := range lines {
		byInvoice[l.InvoiceID] = append(byInvoice[l.InvoiceID], l)
	}
	bills, err := q.ListProviderBillsForInvoices(ctx, gen.ListProviderBillsForInvoicesParams{MerchantID: merchantID, InvoiceIds: ids})
	if err != nil {
		return nil, err
	}
	billsByInvoice := map[uuid.UUID][]billing.InvoiceProviderBill{}
	for _, bill := range bills {
		billsByInvoice[bill.InvoiceID] = append(billsByInvoice[bill.InvoiceID], billing.InvoiceProviderBill{
			PSPID: billing.PSPID(bill.PspID), Kind: bill.SourceKind, Reference: bill.ProviderReference,
			SettlementSource: bill.SettlementSource, NonCashAmount: bill.NonCashAmount, SettledAt: bill.SettledAt,
		})
	}
	out := make([]*Invoice, len(rows))
	for i, r := range rows {
		out[i] = &Invoice{BillingInvoice: r, Lines: byInvoice[r.ID], PriceReferences: refs, ProviderBills: billsByInvoice[r.ID]}
	}
	return out, nil
}

func (s *Service) load(ctx context.Context, q *gen.Queries, merchantID, id uuid.UUID, lock bool) (*Invoice, error) {
	var row gen.BillingInvoice
	var err error
	if lock {
		row, err = q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: merchantID, ID: id})
	} else {
		row, err = q.GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: merchantID, ID: id})
	}
	if db.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.withLines(ctx, q, merchantID, row)
}

func (s *Service) withLines(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, row gen.BillingInvoice) (*Invoice, error) {
	invoices, err := s.attachLines(ctx, q, merchantID, []gen.BillingInvoice{row})
	if err != nil {
		return nil, err
	}
	return invoices[0], nil
}

// CancelMerchantInvoice withdraws an open invoice the merchant created.
func (s *Service) CancelMerchantInvoice(ctx context.Context, id uuid.UUID) (*Invoice, error) {
	invoice, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if invoice.Origin != string(billing.InvoiceOriginMerchant) {
		return nil, ErrNotCancelable
	}
	return s.Cancel(ctx, invoice.CustomerID, id)
}

// Cancel closes customer's open invoice or one awaiting their action, and
// releases its claims. A canceled invoice that is paid late revives (Paid).
func (s *Service) Cancel(ctx context.Context, customerID, id uuid.UUID) (*Invoice, error) {
	if err := s.close(ctx, customerID, id, billing.InvoiceCanceled); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// ExpireIfDue materializes an invoice's elapsed expiry when a command replays
// or confirms it. close rechecks under the invoice lock, so a concurrent payment
// that has reached processing or completion keeps its invoice and claims.
func (s *Service) ExpireIfDue(ctx context.Context, invoice *Invoice) (*Invoice, error) {
	if !Expired(invoice.BillingInvoice, s.now()) {
		return invoice, nil
	}
	if err := s.close(ctx, invoice.CustomerID, invoice.ID, billing.InvoiceExpired); err != nil {
		return nil, err
	}
	return s.GetForCustomer(ctx, invoice.CustomerID, invoice.ID)
}

// close moves a live invoice to canceled or expired. A zero customerID is the
// system (expiry).
func (s *Service) close(ctx context.Context, customerID, id uuid.UUID, status billing.InvoiceStatus) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	var awaiting *uuid.UUID
	var refused error
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.DB.NewWithPgxTx(tx)
		invoice, err := s.load(ctx, d.Gen(ctx), mid.UUID(), id, true)
		if err != nil {
			return err
		}
		if customerID != uuid.Nil && invoice.CustomerID != customerID {
			return ErrNotFound
		}
		if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
			if status == billing.InvoiceExpired {
				return nil
			}
			return ErrNotCancelable
		}
		if invoice.Origin == string(billing.InvoiceOriginSubscription) {
			if status == billing.InvoiceExpired {
				return nil
			}
			return ErrRenewalCancel
		}
		if invoice.Status != string(billing.InvoiceOpen) {
			if status == billing.InvoiceExpired {
				return nil
			}
			return ErrNotCancelable
		}
		now := s.now()
		if status == billing.InvoiceExpired && !Expired(invoice.BillingInvoice, now) {
			return nil
		}
		if status == billing.InvoiceCanceled && Expired(invoice.BillingInvoice, now) {
			// Its expiry closed it first.
			status, refused = billing.InvoiceExpired, ErrNotCancelable
		}
		awaiting, err = s.closeLocked(ctx, d, invoice, status, now)
		return err
	})
	if err == nil && awaiting != nil && s.Abandon != nil {
		if err := s.Abandon(ctx, *awaiting); err != nil {
			s.Log.WarnContext(ctx, "closed invoice's challenged payment is closed by its verifier later", logx.Err(err), "invoice_id", id)
		}
	}
	if err != nil {
		return err
	}
	return refused
}

// closeLocked closes a locked open invoice in d's transaction, and reports the
// attempt awaiting its customer, which the caller abandons once committed.
func (s *Service) closeLocked(ctx context.Context, d *db.DB, invoice *Invoice, status billing.InvoiceStatus, now time.Time) (*uuid.UUID, error) {
	q := d.Gen(ctx)
	if _, err := q.CloseInvoice(ctx, gen.CloseInvoiceParams{MerchantID: invoice.MerchantID, ID: invoice.ID, Status: string(status), Now: now}); err != nil {
		return nil, err
	}
	var awaiting *uuid.UUID
	if invoice.AwaitsCustomer() {
		awaiting = invoice.AttemptID
	}
	if invoice.AttemptID != nil {
		if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: invoice.MerchantID, ID: *invoice.AttemptID, Status: attemptClosed(status), Now: now}); err != nil {
			return nil, err
		}
	}
	if _, err := q.ReleaseInvoiceClaims(ctx, gen.ReleaseInvoiceClaimsParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, InvoiceID: invoice.ID}); err != nil {
		return nil, err
	}
	invoice.Status, invoice.PaymentStatus = string(status), string(billing.InvoicePaymentRequiresPaymentMethod)
	if err := s.event(ctx, q, &invoice.BillingInvoice, "invoice."+string(status), now, nil); err != nil {
		return nil, err
	}
	return awaiting, notificationinbox.WithdrawInvoice(ctx, d, invoice.CustomerID, invoice.ID)
}

func attemptClosed(status billing.InvoiceStatus) string {
	if status == billing.InvoiceExpired {
		return "expired"
	}
	return "canceled"
}

// event enqueues one host event of an invoice transition; invoice is the row as
// the transition leaves it.
func (s *Service) event(ctx context.Context, q *gen.Queries, invoice *gen.BillingInvoice, kind string, at time.Time, extra map[string]any) error {
	return invoiceEvent(ctx, q, invoice, kind, at, extra)
}

// invoiceEvent records one invoice transition as a host event.
func invoiceEvent(ctx context.Context, q *gen.Queries, invoice *gen.BillingInvoice, kind string, at time.Time, extra map[string]any) error {
	return invoiceevent.Enqueue(ctx, q, invoice, kind, at, extra)
}

// ExpireDue expires every live invoice of the merchant past its expiry, a page
// of batch at a time, until none is left or ctx ends.
func (s *Service) ExpireDue(ctx context.Context, batch int32) (int, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	args := gen.ListExpiredInvoicesParams{MerchantID: mid.UUID(), Now: now, RowLimit: batch}
	expired := 0
	for {
		if err := ctx.Err(); err != nil {
			return expired, err
		}
		rows, err := s.DB.Gen(ctx).ListExpiredInvoices(ctx, args)
		if err != nil {
			return expired, err
		}
		for _, row := range rows {
			if err := s.close(ctx, uuid.Nil, row.ID, billing.InvoiceExpired); err != nil {
				return expired, fmt.Errorf("expire invoice %s: %w", row.ID, err)
			}
			expired++
		}
		if len(rows) < int(batch) {
			return expired, nil
		}
		last := rows[len(rows)-1]
		args.AfterExpiresAt, args.AfterID = &last.ExpiresAt, &last.ID
	}
}

// ExpiryMerchants lists merchants after the cursor with a live invoice or an
// invoice attempt waiting on its customer past its expiry.
func ExpiryMerchants(ctx context.Context, database *db.DB, now time.Time, after uuid.UUID, limit int32) ([]uuid.UUID, error) {
	return database.GenDirectory().ListInvoiceExpiryMerchants(ctx, gen.ListInvoiceExpiryMerchantsParams{Now: now, AfterMerchantID: after, MerchantLimit: limit})
}

func invoicePriceReferences(ctx context.Context, q *gen.Queries, lines []gen.BillingInvoiceLine) (map[uuid.UUID]billing.PriceID, error) {
	ids := make([]uuid.UUID, 0, len(lines))
	for _, line := range lines {
		if line.PriceID != nil {
			ids = append(ids, *line.PriceID)
		}
	}
	return db.PriceReferences(ctx, q, ids)
}
