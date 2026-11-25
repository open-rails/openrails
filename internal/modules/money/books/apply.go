package books

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	unit "github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ApplyInvoice applies a completed bill's money to its lines, once per line:
// each payment's cash, the invoice's own first, until the bill is paid (DR
// unapplied_cash), then each line's share of the bill's non-cash settlement
// (DR revenue_adjustments), CR deferred_revenue, into a revenue schedule over
// the service the line sells, funded with exactly what it was applied. The
// schedule recognises at once what that service earned by the application. A
// provider's negative adjustment lines reduce the others in proportion; a
// positive one (tax or shipping) is no revenue and waits, unapplied, with a
// finding. Money beyond the bill stays unapplied; a credit line's waits for
// its credit lot, and a usage invoice's settles its receivable. A purchase's
// timed line with no access granted is left unapplied with a finding.
// services names the service of a line its caller knows better than the invoice
// does.
func ApplyInvoice(ctx context.Context, d *db.DB, invoiceID uuid.UUID, services map[uuid.UUID]Service) error {
	return apply(ctx, d, invoiceID, services, true)
}

// ApplyRecorded applies a provider bill as it is recorded: as ApplyInvoice,
// except that a timed line waits silently for the access its caller grants
// next, when the caller applies the invoice again (ApplyInvoice).
func ApplyRecorded(ctx context.Context, d *db.DB, invoiceID uuid.UUID) error {
	return apply(ctx, d, invoiceID, nil, false)
}

func apply(ctx context.Context, d *db.DB, invoiceID uuid.UUID, services map[uuid.UUID]Service, final bool) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := d.Gen(ctx)
	// One application of an invoice at a time: each allocates what the ones
	// before it left.
	invoice, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: mid.UUID(), ID: invoiceID})
	if err != nil {
		return err
	}
	if invoice.Status != "complete" || invoice.CompletedAt == nil || invoice.Origin == "usage" || invoice.Total <= 0 {
		return nil
	}
	if CheckCurrency(invoice.Currency) != nil {
		if !unit.IsUnitCode(invoice.Currency) {
			return nil
		}
		return applyUnitInvoice(ctx, d, q, invoice, services, final)
	}
	lines, err := q.ListInvoiceLines(ctx, gen.ListInvoiceLinesParams{MerchantID: mid.UUID(), InvoiceIds: []uuid.UUID{invoiceID}})
	if err != nil {
		return err
	}
	receipts, err := q.ListInvoiceReceipts(ctx, gen.ListInvoiceReceiptsParams{MerchantID: mid.UUID(), InvoiceID: invoiceID})
	if err != nil {
		return err
	}
	nonCash, err := q.GetInvoiceNonCashSettlement(ctx, gen.GetInvoiceNonCashSettlementParams{MerchantID: mid.UUID(), InvoiceID: invoiceID})
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		ids[i] = l.ID
	}
	scheduled, err := q.ListScheduledInvoiceLines(ctx, gen.ListScheduledInvoiceLinesParams{MerchantID: mid.UUID(), InvoiceLineIds: ids})
	if err != nil {
		return err
	}
	payments := make([]string, len(receipts))
	for i, p := range receipts {
		payments[i] = p.ID.String()
	}
	earlier, err := q.ListInvoiceApplications(ctx, gen.ListInvoiceApplicationsParams{MerchantID: mid.UUID(), PaymentIds: payments, InvoiceID: invoiceID.String()})
	if err != nil {
		return err
	}
	applied := map[string]int64{}
	for _, e := range earlier {
		applied[e.Source+":"+e.SourceID] = e.Applied
	}
	amounts, done := make([]int64, len(lines)), make([]bool, len(lines))
	var unbooked int64
	for i, l := range lines {
		if pendingTax(l) {
			unbooked += l.Amount
			continue
		}
		// A credit line is its lot's to apply (books.Books.IssueCredit): it
		// takes nothing here, and what its lot applied of a payment counts
		// as used.
		credit, err := creditLine(ctx, q, l)
		if err != nil {
			return err
		}
		if credit {
			// Nor does a provider's discount reach it: the lot's price is
			// its own.
			done[i] = true
			continue
		}
		amounts[i], done[i] = l.Amount, slices.Contains(scheduled, l.ID)
	}
	// What each payment's earlier applications and reversals took of it.
	paid, before := make([]int64, len(receipts)), make([]int64, len(receipts))
	for i, p := range receipts {
		pb, err := paymentBooksOf(ctx, q, mid.UUID(), p.ID)
		if err != nil {
			return err
		}
		paid[i], before[i] = p.Amount, pb.used()
	}
	a := allocateRest(amounts, done, paid, before, nonCash, applied["order:"+invoiceID.String()])
	// The bill is paid when the last money it took arrived, and applied when
	// it completed, whichever is first.
	var last time.Time
	for r, p := range receipts {
		if sum(a.cash[r]) > 0 && p.PurchasedAt.After(last) {
			last = p.PurchasedAt
		}
	}
	at := *invoice.CompletedAt
	if !last.IsZero() && last.Before(at) {
		at = last
	}
	at = at.UTC().Truncate(time.Microsecond)

	type made struct {
		index int
		row   gen.BillingRevenueSchedule
		steps []recognition
	}
	var schedules []made
	var loc *time.Location
	for i, line := range lines {
		funded := a.nonCash[i]
		for r := range receipts {
			funded += a.cash[r][i]
		}
		if funded <= 0 {
			continue
		}
		plan, ready, err := planLine(ctx, q, invoice, line, services, at)
		if err != nil {
			return fmt.Errorf("books: invoice line %s: %w", line.ID, err)
		}
		if !ready {
			if final {
				if err := unappliedLine(ctx, q, invoice, line); err != nil {
					return err
				}
			}
			continue
		}
		if plan.method == Ratable && loc == nil {
			if loc, err = AccountingZone(ctx, d); err != nil {
				return err
			}
		}
		params, steps := newSchedule(invoice, line, plan, uuidutil.NewV7(), invoice.Currency, funded, at, loc)
		row, err := q.CreateRevenueSchedule(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := q.ResolveStandingFinding(ctx, gen.ResolveStandingFindingParams{MerchantID: mid.UUID(), FindingType: findingUnappliedLine, SubjectKey: "order_line:" + line.ID.String()}); err != nil {
			return err
		}
		schedules = append(schedules, made{index: i, row: row, steps: steps})
	}
	if unbooked > 0 {
		if err := unbookedAdjustment(ctx, q, invoice, unbooked); err != nil {
			return err
		}
	}
	if len(schedules) == 0 {
		return nil
	}

	// Every transaction takes the general ledger's accounts in one order:
	// receipts, cash applied, non-cash, then recognition. A receipt was booked
	// when its payment was stored; one stored before the books is booked now.
	b := New(q, mid.UUID())
	for r, p := range receipts {
		if sum(a.cash[r]) > 0 {
			if err := ReceivePayment(ctx, q, p); err != nil {
				return err
			}
		}
	}
	for r, p := range receipts {
		for _, m := range schedules {
			if amount := a.cash[r][m.index]; amount > 0 {
				if err := b.fund(ctx, m.row, Ref{Type: UnappliedCash}, CashApplied, Coord{Operation: OpApplication, Source: "payment", SourceID: p.ID.String()}, amount, at); err != nil {
					return err
				}
			}
		}
	}
	for _, m := range schedules {
		if amount := a.nonCash[m.index]; amount > 0 {
			if err := b.fund(ctx, m.row, Ref{Type: RevenueAdjustments}, RevenueAdjusted, Coord{Operation: OpApplication, Source: "order", SourceID: invoice.ID.String()}, amount, at); err != nil {
				return err
			}
		}
	}
	for _, m := range schedules {
		if err := b.recognize(ctx, m.row, m.steps); err != nil {
			return err
		}
	}
	return nil
}

// applyUnitInvoice schedules the revenue of an invoice priced in a merchant's own
// unit, which the customer's balance paid: each line's spends released its
// lots' money into deferred_revenue, in each lot's paid currency, naming the
// schedule CreditLineSchedule(line, currency). One schedule per currency
// recognises it over the line's service, funded with exactly what the spends
// released; the units book's spends were the invoice's payment.
func applyUnitInvoice(ctx context.Context, d *db.DB, q *gen.Queries, invoice gen.BillingInvoice, services map[uuid.UUID]Service, final bool) error {
	lines, err := q.ListInvoiceLines(ctx, gen.ListInvoiceLinesParams{MerchantID: invoice.MerchantID, InvoiceIds: []uuid.UUID{invoice.ID}})
	if err != nil {
		return err
	}
	ids := make([]string, len(lines))
	for i, l := range lines {
		ids[i] = l.ID.String()
	}
	released, err := q.ListCreditLineReleases(ctx, gen.ListCreditLineReleasesParams{MerchantID: invoice.MerchantID, InvoiceLineIds: ids})
	if err != nil {
		return err
	}
	schedules := make([]uuid.UUID, len(released))
	for i, r := range released {
		schedules[i] = r.RevenueScheduleID
	}
	money, err := q.ListScheduleLotMoney(ctx, gen.ListScheduleLotMoneyParams{MerchantID: invoice.MerchantID, RevenueScheduleIds: schedules, InvoiceLineIds: ids})
	if err != nil {
		return err
	}
	held := make(map[uuid.UUID]int64, len(money))
	for _, m := range money {
		held[m.RevenueScheduleID] = m.Held
	}
	at := invoice.CompletedAt.UTC().Truncate(time.Microsecond)
	b := New(q, invoice.MerchantID)
	var loc *time.Location
	for _, line := range lines {
		for _, r := range released {
			if r.InvoiceLineID != line.ID || r.Amount <= 0 {
				continue
			}
			if r.RevenueScheduleID != CreditLineSchedule(line.ID, r.Currency) {
				return fmt.Errorf("books: invoice line %s released %s under schedule %s", line.ID, r.Currency, r.RevenueScheduleID)
			}
			// Money its reversals took all of back has nothing to schedule.
			if held[r.RevenueScheduleID] <= 0 {
				continue
			}
			plan, ready, err := planLine(ctx, q, invoice, line, services, at)
			if err != nil {
				return fmt.Errorf("books: invoice line %s: %w", line.ID, err)
			}
			if !ready {
				if final {
					if err := unappliedLine(ctx, q, invoice, line); err != nil {
						return err
					}
				}
				continue
			}
			if plan.method == Ratable && loc == nil {
				if loc, err = AccountingZone(ctx, d); err != nil {
					return err
				}
			}
			params, steps := newSchedule(invoice, line, plan, r.RevenueScheduleID, r.Currency, held[r.RevenueScheduleID], at, loc)
			row, err := q.CreateRevenueSchedule(ctx, params)
			if errors.Is(err, pgx.ErrNoRows) {
				// Money released into it since it was made adds to its funding.
				if loc == nil {
					if loc, err = AccountingZone(ctx, d); err != nil {
						return err
					}
				}
				if err := b.fundMore(ctx, r.RevenueScheduleID, held[r.RevenueScheduleID], r.Amount, maxTime(at, r.ReleasedAt.UTC().Truncate(time.Microsecond)), loc); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if _, err := q.ResolveStandingFinding(ctx, gen.ResolveStandingFindingParams{MerchantID: invoice.MerchantID, FindingType: findingUnappliedLine, SubjectKey: "order_line:" + line.ID.String()}); err != nil {
				return err
			}
			if err := b.recognize(ctx, row, steps); err != nil {
				return err
			}
		}
	}
	return nil
}

// pendingTax is a provider's positive adjustment line: its tax, discounts and
// shipping beyond the catalog lines. It is no revenue, and the tax in it is
// not yet told apart.
func pendingTax(l gen.BillingInvoiceLine) bool {
	return l.Amount > 0 && l.Source != nil && *l.Source == "provider_adjustment"
}

const (
	findingUnappliedLine       = "books.line.unapplied"
	findingUnappliedAdjustment = "books.adjustment.unapplied"
)

// unappliedLine records a paid timed line whose access was never granted: its
// money stays unapplied, with no service to recognise over.
func unappliedLine(ctx context.Context, q *gen.Queries, invoice gen.BillingInvoice, line gen.BillingInvoiceLine) error {
	evidence, err := json.Marshal(map[string]any{"invoice_id": invoice.ID.String(), "invoice_line_id": line.ID.String(), "currency": invoice.Currency, "amount": strconv.FormatInt(line.Amount, 10)})
	if err != nil {
		return err
	}
	action := "The line's timed access was never granted, so its money stays in unapplied_cash: grant the access and apply the invoice again, or refund it."
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: invoice.MerchantID, FindingType: findingUnappliedLine,
		SubjectKey: "order_line:" + line.ID.String(), Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
	return err
}

// unbookedAdjustment records that money a provider billed beyond the catalog
// lines stays unapplied until its tax is booked: raised once, so a later
// application never reopens one a person settled.
func unbookedAdjustment(ctx context.Context, q *gen.Queries, invoice gen.BillingInvoice, amount int64) error {
	subject := "order:" + invoice.ID.String()
	_, err := q.GetPSPFindingByIdentity(ctx, gen.GetPSPFindingByIdentityParams{MerchantID: invoice.MerchantID, FindingType: findingUnappliedAdjustment, SubjectKey: subject})
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	evidence, err := json.Marshal(map[string]any{"invoice_id": invoice.ID.String(), "currency": invoice.Currency, "amount": strconv.FormatInt(amount, 10)})
	if err != nil {
		return err
	}
	action := "The provider billed tax, discounts or shipping beyond the invoice's catalog lines; that money stays in unapplied_cash until its tax is booked."
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: invoice.MerchantID, FindingType: findingUnappliedAdjustment,
		SubjectKey: subject, Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
	return err
}

// fund credits a schedule's deferred revenue from debit.
func (b *Books) fund(ctx context.Context, row gen.BillingRevenueSchedule, debit Ref, typ TransferType, coord Coord, amount int64, at time.Time) error {
	_, _, err := b.Post(ctx, Posting{
		Debit: debit, Credit: Ref{Type: DeferredRevenue}, Amount: amount, Currency: row.Currency, Type: typ, Coord: coord, EffectiveAt: at,
		Customer: &row.CustomerID, Invoice: &row.InvoiceID, InvoiceLine: &row.InvoiceLineID, Product: row.ProductID, RevenueSchedule: &row.ID,
	})
	return err
}

// recognize posts a schedule's recognitions: DR deferred_revenue / CR its
// revenue account (a reinstatement the other way), each at (operation,
// schedule, instant). The schedule's lock makes each one new: one already
// posted means the schedule and its transfers disagree.
func (b *Books) recognize(ctx context.Context, row gen.BillingRevenueSchedule, steps []recognition) error {
	for _, st := range steps {
		debit, credit := Ref{Type: DeferredRevenue}, Ref{Type: AccountType(row.RevenueAccount)}
		if st.op == OpReinstatement {
			debit, credit = credit, debit
		}
		coord := Coord{Operation: st.op, Source: "revenue_schedule", SourceID: row.ID.String() + "@" + st.at.UTC().Format(time.RFC3339Nano)}
		if st.op == OpTermination || st.op == OpReinstatement {
			coord.SourceID += "#" + strconv.Itoa(st.gen)
		}
		_, applied, err := b.Post(ctx, Posting{
			Debit: debit, Credit: credit, Amount: st.amount, Currency: row.Currency, Type: RevenueRecognized, Coord: coord,
			EffectiveAt: st.dated, Customer: &row.CustomerID, Invoice: &row.InvoiceID, InvoiceLine: &row.InvoiceLineID, Product: row.ProductID, RevenueSchedule: &row.ID,
		})
		if err != nil {
			return err
		}
		if !applied {
			return fmt.Errorf("%w: revenue schedule %s already posted %s", ErrCoordinateReused, row.ID, coord)
		}
	}
	return nil
}

// allocateRest allocates what a bill's earlier applications left: to its
// lines not done, each payment's cash and the non-cash settlement less what
// was applied of them. No payment is applied beyond what it brought, however
// its receipts changed between applications.
func allocateRest(amounts []int64, done []bool, receipts, applied []int64, nonCash, nonCashApplied int64) allocation {
	left := make([]int64, len(receipts))
	for r, amount := range receipts {
		left[r] = max(amount-applied[r], 0)
	}
	return allocate(amounts, done, left, max(nonCash-nonCashApplied, 0))
}

// allocation is a bill's money per line: its net price, and the parts of it
// the non-cash settlement and each payment paid.
type allocation struct {
	net, nonCash []int64
	cash         [][]int64
}

// allocate spreads a bill's negative lines over its positive ones, then, over
// the lines not done, its non-cash settlement and each payment in turn, until
// none lacks anything. A done line was applied before: it takes nothing.
func allocate(amounts []int64, done []bool, receipts []int64, nonCash int64) allocation {
	a := allocation{net: make([]int64, len(amounts)), cash: make([][]int64, len(receipts))}
	positive := make([]int64, len(amounts))
	var negative int64
	for i, v := range amounts {
		if v > 0 {
			positive[i] = v
		} else {
			negative -= v
		}
	}
	off := spread(negative, positive)
	open := make([]int64, len(amounts))
	var total int64
	for i, v := range positive {
		a.net[i] = max(v-off[i], 0)
		if !done[i] {
			open[i] = a.net[i]
			total += open[i]
		}
	}
	a.nonCash = spread(min(nonCash, total), open)
	need := make([]int64, len(amounts))
	var left int64
	for i := range need {
		need[i] = open[i] - a.nonCash[i]
		left += need[i]
	}
	for r, amount := range receipts {
		applied := min(amount, left)
		a.cash[r] = spread(applied, need)
		for i, part := range a.cash[r] {
			need[i] -= part
		}
		left -= applied
	}
	return a
}

func sum(parts []int64) int64 {
	var s int64
	for _, p := range parts {
		s += p
	}
	return s
}

// creditLine reports a line that buys credit: its money is its credit lot's.
func creditLine(ctx context.Context, q *gen.Queries, line gen.BillingInvoiceLine) (bool, error) {
	if granted(line.CreditGrant) {
		return true, nil
	}
	if line.ProductID == nil {
		return false, nil
	}
	product, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: line.MerchantID, ID: *line.ProductID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && granted(product.CreditGrant), err
}

func granted(raw []byte) bool { return len(raw) > 0 && string(raw) != "null" }

// linePlan is how a line's revenue is recognised.
// newSchedule is a line's schedule in currency as its application makes it,
// funded with funded and recognised to at, and the recognition that
// application posts: one builder for a bill's lines and a unit invoice's.
func newSchedule(invoice gen.BillingInvoice, line gen.BillingInvoiceLine, plan linePlan, id uuid.UUID, currency string, funded int64, at time.Time, loc *time.Location) (gen.CreateRevenueScheduleParams, []recognition) {
	s := schedule{service: plan.service, funded: funded, through: plan.service.Starts, ended: plan.ended}
	steps, next := s.catchUp(at, loc)
	params := gen.CreateRevenueScheduleParams{
		MerchantID: invoice.MerchantID, ID: id, CustomerID: invoice.CustomerID, Currency: currency, InvoiceID: invoice.ID, InvoiceLineID: line.ID,
		SubscriptionID: plan.subscription, ProductAccessID: plan.access, ProductID: line.ProductID, PriceID: line.PriceID,
		Method: string(plan.method), RevenueAccount: string(plan.account), Funded: s.funded, RecognizedNet: s.recognized,
		ServiceStartsAt: plan.service.Starts, ServiceEndsAt: plan.service.Ends, EndedAt: s.ended, RecognizedThroughAt: s.through, NextRecognitionAt: next,
		Forfeited: s.forfeited,
	}
	if plan.service.Months > 1 {
		months := int32(plan.service.Months) // #nosec G115 -- a calendar interval is at most 1200 months
		params.ServiceMonths = &months
	}
	return params, steps
}

type linePlan struct {
	method       Method
	account      AccountType
	service      Service
	subscription *uuid.UUID
	access       *uuid.UUID
	ended        *time.Time
}

// planLine is the service a paid line sells: a subscription's period
// (subscription revenue, ratable, by the month for a calendar price), a
// purchase's access window (one-time revenue, ratable), or else delivery at
// once (one-time revenue, point in time). ready is false for timed access not
// yet granted. An end already reached (a subscription's, revoked access)
// carries over.
func planLine(ctx context.Context, q *gen.Queries, invoice gen.BillingInvoice, line gen.BillingInvoiceLine, services map[uuid.UUID]Service, at time.Time) (p linePlan, ready bool, err error) {
	p = linePlan{method: Ratable, account: OneTimeRevenue}
	switch {
	case line.BillingIntervalHours != nil || line.BillingIntervalMonths != nil || invoice.PeriodStartsAt != nil:
		p.account, p.subscription = SubscriptionRevenue, line.SubscriptionID
		if p.subscription == nil {
			p.subscription = invoice.SubscriptionID
		}
		months := 0
		if line.BillingIntervalMonths != nil {
			months = int(*line.BillingIntervalMonths)
		}
		switch svc, known := services[line.ID]; {
		case known:
			p.service = svc
		case invoice.PeriodStartsAt != nil && invoice.PeriodEndsAt != nil:
			p.service = Service{Starts: *invoice.PeriodStartsAt, Ends: *invoice.PeriodEndsAt, Months: months}
		case months > 0:
			p.service = Service{Starts: *invoice.CompletedAt, Ends: calendarutil.AddMonths(*invoice.CompletedAt, months), Months: months}
		case line.BillingIntervalHours != nil:
			p.service = Service{Starts: *invoice.CompletedAt, Ends: invoice.CompletedAt.Add(time.Duration(*line.BillingIntervalHours) * time.Hour)}
		}
		p.service = p.service.calendar()
		// An end before the schedule existed (a charge after cancellation, a
		// subscription imported canceled) reaches it here; later ones by
		// trigger, and an undone one is taken back. A replaced subscription
		// (an upgrade) keeps its periods running.
		if p.subscription != nil {
			sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: invoice.MerchantID, ID: *p.subscription})
			switch {
			case err == nil && sub.EndedAt != nil && (sub.CancelType == nil || *sub.CancelType != "upgrade"):
				p.ended = sub.EndedAt
			case err != nil && !errors.Is(err, pgx.ErrNoRows):
				return p, false, err
			}
		}
	case line.AccessDurationHours != nil:
		var access gen.BillingProductAccess
		switch {
		case line.ProductAccessID != nil:
			access, err = q.GetProductAccessByID(ctx, gen.GetProductAccessByIDParams{MerchantID: invoice.MerchantID, ID: *line.ProductAccessID})
		case invoice.PaymentID != nil && line.ProductID != nil:
			access, err = q.GetPurchaseAccessByPayment(ctx, gen.GetPurchaseAccessByPaymentParams{MerchantID: invoice.MerchantID, PaymentID: *invoice.PaymentID, ProductID: *line.ProductID})
		default:
			return p, false, nil
		}
		if errors.Is(err, pgx.ErrNoRows) || err == nil && access.EndsAt == nil {
			return p, false, nil
		}
		if err != nil {
			return p, false, err
		}
		p.access, p.service, p.ended = &access.ID, Service{Starts: access.StartsAt, Ends: *access.EndsAt}, access.RevokedAt
	default:
		p.method, p.service = PointInTime, Service{Starts: at, Ends: at}
	}
	if p.method == Ratable && !p.service.Ends.After(p.service.Starts) {
		return p, false, fmt.Errorf("no service period to recognise over")
	}
	// An end at or after the service's own is no early end.
	if p.ended != nil {
		if !p.ended.Before(p.service.Ends) {
			p.ended = nil
		} else {
			e := maxTime(*p.ended, p.service.Starts)
			p.ended = &e
		}
	}
	return p, true, nil
}
