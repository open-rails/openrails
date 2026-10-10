package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	log "github.com/sirupsen/logrus"
)

// Preview prices lines for customer without writing.
func (s *Service) Preview(ctx context.Context, customerID uuid.UUID, lines []billing.OrderLineParams) (*Quote, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.quote(ctx, s.DB.Gen(ctx), mid.UUID(), customerID, lines, uuid.Nil, s.now())
}

// CreateInput is a new order. IdempotencyKey is the request's scoped key,
// kept on the order with the request's digest.
type CreateInput struct {
	CustomerID     uuid.UUID
	Origin         billing.OrderOrigin
	Lines          []billing.OrderLineParams
	ExpectedTotal  *int64
	IdempotencyKey string
	RequestDigest  []byte
	TTL            time.Duration
}

// Create freezes lines into an order and claims what its unique lines buy.
// A refused line is a LineError; a total other than ExpectedTotal is
// ErrTotalChanged. An order with nothing to pay is paid at once.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Order, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if in.CustomerID == uuid.Nil || in.TTL <= 0 || (in.IdempotencyKey == "") != (len(in.RequestDigest) == 0) {
		return nil, errors.New("order needs a customer, a lifetime and a keyed digest")
	}
	id := uuidutil.NewV7()
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
		now := s.now()
		quote, err := s.quote(ctx, q, mid.UUID(), in.CustomerID, in.Lines, uuid.Nil, now)
		if err != nil {
			return err
		}
		if i := quote.Refused(); i >= 0 {
			return &LineError{Index: i, Refusal: *quote.Lines[i].Refusal}
		}
		if in.ExpectedTotal != nil && *in.ExpectedTotal != quote.Total {
			return ErrTotalChanged
		}
		var key *string
		if in.IdempotencyKey != "" {
			key = &in.IdempotencyKey
		}
		if err := q.CreateOrder(ctx, gen.CreateOrderParams{MerchantID: mid.UUID(), ID: id, CustomerID: in.CustomerID, Origin: string(in.Origin),
			Status: string(billing.OrderOpen), Currency: quote.Currency, Total: quote.Total, IdempotencyKey: key, RequestDigest: in.RequestDigest,
			ExpiresAt: now.Add(in.TTL), Now: now}); err != nil {
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
			access, err := int32Ptr(l.Price.AccessDurationHours)
			if err != nil {
				return err
			}
			if err := q.CreateOrderLine(ctx, gen.CreateOrderLineParams{MerchantID: mid.UUID(), ID: uuidutil.NewV7(), OrderID: id, CustomerID: in.CustomerID,
				Position: int32(i), PriceID: l.Price.ID, ProductID: l.Product.ID, Description: l.Product.DisplayName, Quantity: quantity,
				UnitAmount: l.UnitAmount, Amount: l.Amount, Ownership: string(l.Ownership), ClaimKey: claim,
				BillingIntervalHours: interval, AccessDurationHours: access,
				CreditGrant: credit, Now: now}); err != nil {
				return err
			}
			if l.ClaimKey == "" {
				continue
			}
			n, err := q.ClaimOwnership(ctx, gen.ClaimOwnershipParams{MerchantID: mid.UUID(), CustomerID: in.CustomerID, ClaimKey: l.ClaimKey, OrderID: id, Now: now})
			if err != nil {
				return err
			}
			if n == 0 {
				held, err := q.GetOwnershipClaim(ctx, gen.GetOwnershipClaimParams{MerchantID: mid.UUID(), CustomerID: in.CustomerID, ClaimKey: l.ClaimKey})
				if err != nil {
					return err
				}
				return &LineError{Index: i, Refusal: *claimRefusal(held)}
			}
		}
		if quote.Total > 0 {
			return nil
		}
		order, err := s.load(ctx, q, mid.UUID(), id, true)
		if err != nil {
			return err
		}
		_, err = s.settle(ctx, d, order, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Get reads an order of the context's merchant.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Order, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.DB.Gen(ctx), mid.UUID(), id, false)
}

// GetForCustomer reads customer's own order; another's does not exist.
func (s *Service) GetForCustomer(ctx context.Context, customerID, id uuid.UUID) (*Order, error) {
	order, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if order.CustomerID != customerID {
		return nil, ErrNotFound
	}
	return order, nil
}

// ByIdempotencyKey is customer's order a scoped create key made, nil when
// none.
func (s *Service) ByIdempotencyKey(ctx context.Context, customerID uuid.UUID, key string) (*Order, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.DB.Gen(ctx).GetOrderByIdempotencyKey(ctx, gen.GetOrderByIdempotencyKeyParams{MerchantID: mid.UUID(), CustomerID: customerID, IdempotencyKey: key})
	if db.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.withLines(ctx, s.DB.Gen(ctx), mid.UUID(), row)
}

// ListFilter narrows a page of orders; a nil field does not filter.
type ListFilter struct {
	CustomerID *uuid.UUID
	PriceID    *uuid.UUID
	Status     *string
}

// List pages the merchant's orders, newest first.
func (s *Service) List(ctx context.Context, filter ListFilter, page billing.PageRequest) (billing.ListPage[*Order], error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return billing.ListPage[*Order]{}, err
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return billing.ListPage[*Order]{}, err
	}
	at, after, err := pagination.After(page.Cursor)
	if err != nil {
		return billing.ListPage[*Order]{}, err
	}
	q := s.DB.Gen(ctx)
	rows, err := q.ListOrdersPage(ctx, gen.ListOrdersPageParams{MerchantID: mid.UUID(), CustomerID: filter.CustomerID, PriceID: filter.PriceID, Status: filter.Status, AfterCreatedAt: at, AfterID: after, RowLimit: pagination.Fetch(limit)})
	if err != nil {
		return billing.ListPage[*Order]{}, err
	}
	cut := pagination.Cut(rows, limit, func(r gen.BillingOrder) any { return pagination.TimeID{At: r.CreatedAt, ID: r.ID} })
	items, err := s.attachLines(ctx, q, mid.UUID(), cut.Items)
	if err != nil {
		return billing.ListPage[*Order]{}, err
	}
	return billing.ListPage[*Order]{Items: items, Next: cut.Next}, nil
}

// ListByIDs reads the merchant's named orders, newest first; unknown ones
// are absent.
func (s *Service) ListByIDs(ctx context.Context, ids []uuid.UUID) ([]*Order, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := s.DB.Gen(ctx)
	rows, err := q.ListOrdersByIDs(ctx, gen.ListOrdersByIDsParams{MerchantID: mid.UUID(), Ids: ids})
	if err != nil {
		return nil, err
	}
	return s.attachLines(ctx, q, mid.UUID(), rows)
}

// attachLines reads the lines of a page of orders.
func (s *Service) attachLines(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, rows []gen.BillingOrder) ([]*Order, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	lines, err := q.ListOrderLines(ctx, gen.ListOrderLinesParams{MerchantID: merchantID, OrderIds: ids})
	if err != nil {
		return nil, err
	}
	byOrder := map[uuid.UUID][]gen.BillingOrderLine{}
	for _, l := range lines {
		byOrder[l.OrderID] = append(byOrder[l.OrderID], l)
	}
	out := make([]*Order, len(rows))
	for i, r := range rows {
		out[i] = &Order{BillingOrder: r, Lines: byOrder[r.ID]}
	}
	return out, nil
}

func (s *Service) load(ctx context.Context, q *gen.Queries, merchantID, id uuid.UUID, lock bool) (*Order, error) {
	var row gen.BillingOrder
	var err error
	if lock {
		row, err = q.LockOrder(ctx, gen.LockOrderParams{MerchantID: merchantID, ID: id})
	} else {
		row, err = q.GetOrder(ctx, gen.GetOrderParams{MerchantID: merchantID, ID: id})
	}
	if db.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.withLines(ctx, q, merchantID, row)
}

func (s *Service) withLines(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, row gen.BillingOrder) (*Order, error) {
	lines, err := q.ListOrderLines(ctx, gen.ListOrderLinesParams{MerchantID: merchantID, OrderIds: []uuid.UUID{row.ID}})
	if err != nil {
		return nil, err
	}
	return &Order{BillingOrder: row, Lines: lines}, nil
}

// Cancel closes customer's open order or one awaiting their action, and
// releases its claims. A canceled order that is paid late revives (Paid).
func (s *Service) Cancel(ctx context.Context, customerID, id uuid.UUID) (*Order, error) {
	if err := s.close(ctx, customerID, id, billing.OrderCanceled); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// close moves a live order to canceled or expired. A zero customerID is the
// system (expiry).
func (s *Service) close(ctx context.Context, customerID, id uuid.UUID, status billing.OrderStatus) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	var awaiting *uuid.UUID
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		order, err := s.load(ctx, q, mid.UUID(), id, true)
		if err != nil {
			return err
		}
		if customerID != uuid.Nil && order.CustomerID != customerID {
			return ErrNotFound
		}
		if order.Status != string(billing.OrderOpen) && order.Status != string(billing.OrderRequiresAction) {
			if status == billing.OrderExpired {
				return nil
			}
			return ErrNotCancelable
		}
		now := s.now()
		if status == billing.OrderExpired && order.ExpiresAt.After(now) {
			return nil
		}
		if _, err := q.CloseOrder(ctx, gen.CloseOrderParams{MerchantID: mid.UUID(), ID: id, Status: string(status), Now: now}); err != nil {
			return err
		}
		if order.Status == string(billing.OrderRequiresAction) {
			awaiting = order.AttemptID
		}
		if order.AttemptID != nil {
			if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: mid.UUID(), ID: *order.AttemptID, Status: attemptClosed(status), Now: now}); err != nil {
				return err
			}
		}
		if _, err := q.ReleaseOrderClaims(ctx, gen.ReleaseOrderClaimsParams{MerchantID: mid.UUID(), CustomerID: order.CustomerID, OrderID: id}); err != nil {
			return err
		}
		return s.event(ctx, q, &order.BillingOrder, "order."+string(status), now, nil)
	})
	if err != nil || awaiting == nil || s.Abandon == nil {
		return err
	}
	if err := s.Abandon(ctx, *awaiting); err != nil {
		log.WithContext(ctx).WithError(err).WithField("order_id", id).Warn("closed order's challenged payment is closed by its verifier later")
	}
	return nil
}

func attemptClosed(status billing.OrderStatus) string {
	if status == billing.OrderExpired {
		return "expired"
	}
	return "canceled"
}

// event enqueues one host event of an order transition.
func (s *Service) event(ctx context.Context, q *gen.Queries, order *gen.BillingOrder, kind string, at time.Time, extra map[string]any) error {
	status := kind[len("order."):]
	if kind == "order.payment_failed" {
		status = string(billing.OrderOpen)
	}
	data := map[string]any{"customer_id": order.CustomerID.String(), "status": status}
	for k, v := range extra {
		data[k] = v
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	dedupe := kind + ":" + order.ID.String()
	if order.AttemptID != nil && (kind == "order.requires_action" || kind == "order.payment_failed") {
		dedupe += ":" + order.AttemptID.String()
	}
	_, err = q.EnqueueOrderHostEvent(ctx, gen.EnqueueOrderHostEventParams{MerchantID: order.MerchantID, EventType: kind, OrderID: order.ID,
		Amount: order.Total, Currency: order.Currency, OccurredAt: at, Data: raw, DedupeKey: dedupe})
	return err
}

// ExpireDue expires the merchant's live orders past their expiry, at most
// limit, and purges unpaid closed orders past retention.
func (s *Service) ExpireDue(ctx context.Context, limit int32) (int, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	ids, err := s.DB.Gen(ctx).ListExpiredOrders(ctx, gen.ListExpiredOrdersParams{MerchantID: mid.UUID(), Now: now, RowLimit: limit})
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.close(ctx, uuid.Nil, id, billing.OrderExpired); err != nil {
			return 0, fmt.Errorf("expire order %s: %w", id, err)
		}
	}
	if _, err := s.DB.Gen(ctx).DeleteClosedUnpaidOrders(ctx, gen.DeleteClosedUnpaidOrdersParams{MerchantID: mid.UUID(), Before: now.Add(-PurgeAfter), RowLimit: limit}); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// SweepMerchants lists merchants with a live order past its expiry or an
// unpaid closed order past retention.
func SweepMerchants(ctx context.Context, database *db.DB, now time.Time, limit int32) ([]uuid.UUID, error) {
	return database.GenDirectory().ListOrderSweepMerchants(ctx, gen.ListOrderSweepMerchantsParams{Now: now, PurgeBefore: now.Add(-PurgeAfter), MerchantLimit: limit})
}
