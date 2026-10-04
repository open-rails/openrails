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

// An off-channel payment is idempotent by transaction id: the same terms
// answer the first payment, changed terms are refused (H-040), never
// answered as if they had been recorded.
func TestOffChannelPaymentIdempotency(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.newCustomer()
			price := w.permanent("content:post")
			priceID := price.ID
			client := w.client[tp]
			params := billing.CreateOffChannelPaymentParams{PriceID: priceID, TransactionID: "cash-" + uuid.NewString()}

			first, err := client.CreateOffChannelPayment(t.Context(), c.cid(), params)
			require.NoError(t, err)
			require.Equal(t, []any{billing.PaymentCharge, billing.PaymentSucceeded, int64(2_000_000), "USD"}, []any{first.Kind, first.Status, first.Amount, first.Currency})
			require.NotEqual(t, billing.ChannelRail, first.Channel)
			require.Nil(t, first.Rail)
			require.True(t, c.entitled("content:post"))

			again, err := client.CreateOffChannelPayment(t.Context(), c.cid(), params)
			require.NoError(t, err)
			require.Equal(t, first.ID, again.ID, "the same terms answer the first payment")

			changed := params
			amount := int64(1_000_000)
			changed.Amount = &amount
			_, err = client.CreateOffChannelPayment(t.Context(), c.cid(), changed)
			require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
			requireCode(t, err, http.StatusConflict, "idempotency_key_reused")
			require.Len(t, completed(w.payments(tp, c.id)), 1, "nothing else was recorded")
		})
	}
}

// Lists are cursor pages, newest first: following next_cursor reads every
// row once, and a cursor the server did not issue is refused.
func TestPaymentListsPageByCursor(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	price := w.permanent("content:post")
	priceID := price.ID
	client := w.client[remote]
	var recorded []billing.PaymentID
	for i := range 5 {
		p, err := client.CreateOffChannelPayment(t.Context(), c.cid(), billing.CreateOffChannelPaymentParams{PriceID: priceID, TransactionID: fmt.Sprintf("cash-%d-%s", i, uuid.NewString())})
		require.NoError(t, err)
		recorded = append([]billing.PaymentID{p.ID}, recorded...)
	}

	var read []billing.PaymentID
	page := billing.PageRequest{Limit: 2}
	for pages := 0; ; pages++ {
		require.Less(t, pages, 3)
		got, err := client.ListPayments(t.Context(), billing.ListPaymentsParams{CustomerID: c.cid(), Page: page})
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

	_, err := client.ListPayments(t.Context(), billing.ListPaymentsParams{Page: billing.PageRequest{Cursor: "not-a-cursor"}})
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

// A customer's invoice profile is absent until set; IfAbsent only creates
// one, and a plain set replaces it.
func TestCustomerInvoiceProfile(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	c.saveCard("nmi", visa) // the customer exists
	client := w.client[remote]

	_, err := client.GetCustomerInvoiceProfile(t.Context(), c.cid())
	require.ErrorIs(t, err, billing.ErrNotFound)

	net30 := billing.InvoiceProfile{NetTermsDays: 30, CollectionMethod: billing.CollectSendInvoice, PONumber: "PO-1", BillingContacts: []billing.InvoiceContact{{Email: "ap@example.com"}}}
	got, err := client.SetCustomerInvoiceProfile(t.Context(), c.cid(), billing.SetInvoiceProfileParams{InvoiceProfile: net30, IfAbsent: true})
	require.NoError(t, err)
	require.Equal(t, 30, got.NetTermsDays)

	net60 := net30
	net60.NetTermsDays = 60
	got, err = client.SetCustomerInvoiceProfile(t.Context(), c.cid(), billing.SetInvoiceProfileParams{InvoiceProfile: net60, IfAbsent: true})
	require.NoError(t, err)
	require.Equal(t, 30, got.NetTermsDays, "IfAbsent answers the existing profile unchanged")

	got, err = client.SetCustomerInvoiceProfile(t.Context(), c.cid(), billing.SetInvoiceProfileParams{InvoiceProfile: net60})
	require.NoError(t, err)
	require.Equal(t, 60, got.NetTermsDays)
	read, err := client.GetCustomerInvoiceProfile(t.Context(), c.cid())
	require.NoError(t, err)
	require.Equal(t, []any{60, "PO-1"}, []any{read.NetTermsDays, read.PONumber})
}

// A card a custodian holds names no PSP (D20): the schema refuses one that
// does, and each charge routes through the one live PSP of the card's rail
// that reaches its custodian, none when no PSP or two do.
func TestCustodianCardRoutesPerCharge(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	c := w.newCustomer()
	held := strings.TrimPrefix(c.saveCard("nmi", visa), "pm_")
	q := gen.New(db.RewriteDBTX(w.pool, w.schema))
	psp := uuid.MustParse(w.psp["nmi"])
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
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.custodians (merchant_id, key, kind, environment, account_id)
		VALUES ($1, 'vault', 'basis_theory', $2, 'bt-e2e') RETURNING id`), merchant, env).Scan(&custodian))
	insert := func(pspID *uuid.UUID) (uuid.UUID, error) {
		var id uuid.UUID
		err := w.pool.QueryRow(ctx, w.q(`INSERT INTO billing.payment_methods (merchant_id, customer_id, rail, psp_id, custodian, custodian_id, rail_method_ref)
			VALUES ($1, $2, 'nmi', $3, 'basis_theory', $4, $5) RETURNING id`), merchant, c.cid().UUID(), pspID, custodian, "tok_"+uuid.NewString()).Scan(&id)
		return id, err
	}
	_, err = insert(&psp)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "payment_methods_psp_custody", pgErr.ConstraintName)
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
