//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// Money received outside OpenRails pays an open order: a manual payment
// idempotent by transaction id, the order paid and fulfilled, and staff find
// it by price and status. An order with a recurring line is refused: its
// renewals need the customer's card.
func TestRecordedOrderPayment(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			life := w.lifetime("recorded:post", 2_000_000)
			client := w.client[tp]
			open := c.order(http.MethodPost, "/orders", "open-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}})
			require.Equal(t, http.StatusCreated, open.status, "%v", open.body)
			order, err := billing.ParseOrderID(open.body["id"].(string))
			require.NoError(t, err)

			short := billing.CreatePaymentParams{OrderID: &order, Amount: 1_000_000, TransactionID: "cash-short-" + uuid.NewString()}
			_, err = client.CreatePayment(t.Context(), short)
			requireCode(t, err, http.StatusConflict, "payment_exceeds_due")

			params := billing.CreatePaymentParams{OrderID: &order, Amount: 2_000_000, TransactionID: "cash-" + uuid.NewString()}
			first, err := client.CreatePayment(t.Context(), params)
			require.NoError(t, err)
			require.Equal(t, []any{billing.PaymentCharge, billing.PaymentSucceeded, int64(2_000_000), "USD", billing.ChannelManual}, []any{first.Kind, first.Status, first.Amount, first.Currency, first.Channel})
			require.Nil(t, first.Rail)
			require.Equal(t, &order, first.OrderID)
			require.True(t, c.entitled("recorded:post"))

			again, err := client.CreatePayment(t.Context(), params)
			require.NoError(t, err)
			require.Equal(t, first.ID, again.ID, "the same terms answer the first payment")
			changed := params
			changed.Amount = 3_000_000
			_, err = client.CreatePayment(t.Context(), changed)
			require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
			requireCode(t, err, http.StatusUnprocessableEntity, "idempotency_key_reused")
			_, err = client.CreatePayment(t.Context(), billing.CreatePaymentParams{OrderID: &order, Amount: 2_000_000, TransactionID: "cash-twice-" + uuid.NewString()})
			requireCode(t, err, http.StatusConflict, "order_not_payable")
			require.Len(t, completed(w.payments(tp, c.id)), 1, "nothing else was recorded")

			paid, err := client.GetOrder(t.Context(), order)
			require.NoError(t, err)
			require.Equal(t, billing.OrderPaid, paid.Status)
			require.NotNil(t, paid.Number)
			bought, err := client.ListOrders(t.Context(), billing.OrderListParams{CustomerID: c.cid(), PriceID: life.ID, Status: billing.OrderPaid})
			require.NoError(t, err)
			require.Len(t, bought.Items, 1, "staff find who bought a price")
			require.Equal(t, order, bought.Items[0].ID)
			byOrder, err := client.ListPayments(t.Context(), billing.PaymentListParams{OrderID: order})
			require.NoError(t, err)
			require.Len(t, byOrder.Items, 1)
			require.Equal(t, first.ID, byOrder.Items[0].ID)

			member := w.membership("recorded:member", 5_000_000)
			recurring := c.order(http.MethodPost, "/orders", "member-"+uuid.NewString(), map[string]any{"lines": []any{line(member, 0)}})
			require.Equal(t, http.StatusCreated, recurring.status, "%v", recurring.body)
			memberOrder, err := billing.ParseOrderID(recurring.body["id"].(string))
			require.NoError(t, err)
			_, err = client.CreatePayment(t.Context(), billing.CreatePaymentParams{OrderID: &memberOrder, Amount: 5_000_000, TransactionID: "cash-member-" + uuid.NewString()})
			requireCode(t, err, http.StatusConflict, "order_has_recurring_line")
		})
	}
}

// Lists are cursor pages, newest first: following next_cursor reads every
// row once, and a cursor the server did not issue is refused.
func TestPaymentListsPageByCursor(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	client := w.client[remote]
	var recorded []billing.PaymentID
	for i := range 5 {
		price := w.permanent(fmt.Sprintf("content:post-%d", i))
		open := c.order(http.MethodPost, "/orders", fmt.Sprintf("open-%d-%s", i, uuid.NewString()), map[string]any{"lines": []any{line(price, 0)}})
		require.Equal(t, http.StatusCreated, open.status, "%v", open.body)
		order, err := billing.ParseOrderID(open.body["id"].(string))
		require.NoError(t, err)
		p, err := client.CreatePayment(t.Context(), billing.CreatePaymentParams{OrderID: &order, Amount: price.UnitAmount, TransactionID: fmt.Sprintf("cash-%d-%s", i, uuid.NewString())})
		require.NoError(t, err)
		recorded = append([]billing.PaymentID{p.ID}, recorded...)
	}

	var read []billing.PaymentID
	page := billing.PageRequest{Limit: 2}
	for pages := 0; ; pages++ {
		require.Less(t, pages, 3)
		got, err := client.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: c.cid(), PageRequest: page})
		require.NoError(t, err)
		for _, p := range got.Items {
			read = append(read, p.ID)
		}
		if got.Next == "" {
			break
		}
		page.Cursor = got.Next
	}
	require.Equal(t, recorded, read, "every payment once, newest first")

	_, err := client.ListPayments(t.Context(), billing.PaymentListParams{PageRequest: billing.PageRequest{Cursor: "not-a-cursor"}})
	requireCode(t, err, http.StatusBadRequest, "invalid_cursor")

	// The customer's own list is the same page shape.
	mine := c.must(http.MethodGet, "/payments?limit=2", "", nil)
	require.Len(t, mine["data"], 2)
	next, _ := mine["next_cursor"].(string)
	require.NotEmpty(t, next)
	rest := c.must(http.MethodGet, "/payments?limit=10&cursor="+next, "", nil)
	require.Len(t, rest["data"], 3)
	require.Nil(t, rest["next_cursor"])
}

// A card a custodian holds names no PSP: the schema refuses one that does,
// and each charge routes through the one live PSP of the card's rail that
// reaches its custodian, none when no PSP or two do.
func TestCustodianCardRoutesPerCharge(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	c := w.newCustomer()
	held := strings.TrimPrefix(c.saveCard("nmi", visa), "pm_")
	q := gen.New(db.RewriteDBTX(w.pool, w.schema))
	psp := w.psp["nmi"].UUID()
	var merchant uuid.UUID
	var env string
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT merchant_id, environment FROM billing.psps WHERE id = $1`), psp).Scan(&merchant, &env))

	method := func(id uuid.UUID) gen.BillingPaymentMethod {
		m, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchant, ID: id})
		require.NoError(t, err)
		return m
	}
	route, err := charge.RoutePSP(ctx, q, method(uuid.MustParse(held)))
	require.NoError(t, err)
	require.Equal(t, psp, route, "a PSP-held card charges through its PSP")

	var custodian uuid.UUID
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.custodians (merchant_id, key, kind, environment, account_id, archived, settings)
		VALUES ($1, 'vault', 'basis_theory', $2, 'bt-e2e', false, '{}') RETURNING id`), merchant, env).Scan(&custodian))
	insert := func(pspID *uuid.UUID) (uuid.UUID, error) {
		var id uuid.UUID
		err := w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.payment_methods (merchant_id, customer_id, rail, psp_id, custodian, custodian_id, rail_method_ref, charge_via, status)
			VALUES ($1, $2, 'nmi', $3, 'basis_theory', $4, $5, 'pan_proxy', 'active') RETURNING id`), merchant, c.cid().UUID(), pspID, custodian, "tok_"+uuid.NewString()).Scan(&id)
		return id, err
	}
	_, err = insert(&psp)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "payment_methods_psp_custody_check", pgErr.ConstraintName)
	vaulted, err := insert(nil)
	require.NoError(t, err)

	_, err = charge.RoutePSP(ctx, q, method(vaulted))
	require.ErrorIs(t, err, charge.ErrNoRoute, "no PSP reaches the custodian")
	_, err = w.pool.Exec(ctx, w.q(`UPDATE billing.psps SET custodian_id = $2 WHERE id = $1`), psp, custodian)
	require.NoError(t, err)
	route, err = charge.RoutePSP(ctx, q, method(vaulted))
	require.NoError(t, err)
	require.Equal(t, psp, route)

	_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.psps (merchant_id, rail, environment, account_id, key, custodian_id) VALUES ($1, 'nmi', $2, $3, 'nmi-two', $4)`),
		merchant, env, "e2e-"+uuid.NewString(), custodian)
	require.NoError(t, err)
	_, err = charge.RoutePSP(ctx, q, method(vaulted))
	require.ErrorIs(t, err, charge.ErrNoRoute, "two PSPs reach it: no single answer")
}
