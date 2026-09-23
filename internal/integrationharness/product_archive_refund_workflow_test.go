//go:build integration

package integrationharness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// refundGateway is a loopback NMI v5 refund endpoint. The original
// transaction id selects the scripted answer: "uncertain-" loses the response
// (HTTP 500) and "decline-" is a definitive decline.
type refundGateway struct {
	*httptest.Server
	mu    sync.Mutex
	calls map[string]int
}

func newRefundGateway(t *testing.T) *refundGateway {
	g := &refundGateway{calls: map[string]int{}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, rest, found := strings.Cut(r.URL.Path, "/payments/")
		txn, ok := strings.CutSuffix(rest, "/refund")
		if r.Method != http.MethodPost || !found || !ok {
			t.Errorf("unexpected refund gateway request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		g.mu.Lock()
		g.calls[txn]++
		n := g.calls[txn]
		g.mu.Unlock()
		switch {
		case strings.HasPrefix(txn, "uncertain-"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasPrefix(txn, "decline-"):
			fmt.Fprint(w, `{"object":"transaction","id":"declined","response":"2","response_code":"200","response_text":"DECLINE"}`)
		default:
			fmt.Fprintf(w, `{"object":"transaction","id":"refund-%s-%d","response":"1","response_text":"SUCCESS"}`, txn, n)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *refundGateway) count(txn string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[txn]
}

func (g *refundGateway) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		n += c
	}
	return n
}

// TestPortableRefundAndProductArchiveWorkflow drives refunds and product
// archives through the portable Client over both topologies: the real
// standalone HTTP server and the embedded in-process runtime, sharing one
// database, merchant and loopback provider.
func TestPortableRefundAndProductArchiveWorkflow(t *testing.T) {
	ctx := t.Context()
	gateway := newRefundGateway(t)
	sandbox := func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL}
		c.ProviderWriteMode = config.ProviderWriteModeFull
		c.SecretBackend = config.SecretBackendDB
		c.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	}
	h := New(t, ctx)
	surface := h.StartStandalone("USD", WithConfig(sandbox))
	owned := surface.ProvisionOwnedMerchant("archive-" + uuid.NewString()[:8])
	remote := surface.Client(openrails.WithMerchantID(owned.MerchantID), openrails.WithAPIKey(owned.APIKey))
	account := "archive-nmi-" + uuid.NewString()[:8]
	SeedPSPs(ctx, t, surface.App().Runtime, owned.MerchantID, config.PSPSet{"nmi": {Rail: "nmi", AccountID: account, NMI: &config.NMIRailConfig{SecurityKey: "synthetic-archive-key"}}})
	embedded := h.StartEmbeddedMerchant("USD", owned.MerchantID, owned.MerchantSlug, sandbox)
	local, err := embedded.Runtime().Client()
	require.NoError(t, err)
	topologies := []struct {
		name   string
		client *openrails.Client
	}{{"remote", remote}, {"embedded", local}}

	pool := h.MerchantPool(owned.MerchantID.UUID())
	var psp uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM billing.psps WHERE merchant_id=$1 AND account_id=$2`, owned.MerchantID.UUID(), account).Scan(&psp))
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `UPDATE billing.psps SET archived=true WHERE merchant_id=$1`, owned.MerchantID.UUID())
		require.NoError(t, err)
	})
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, query, args...)
		require.NoError(t, err)
	}
	type offer struct {
		product *openrails.Product
		price   *openrails.Price
	}
	newOffer := func(client *openrails.Client) offer {
		t.Helper()
		product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "post:" + uuid.NewString(), DisplayName: "Post"})
		require.NoError(t, err)
		price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
		require.NoError(t, err)
		return offer{product, price}
	}
	// purchase records a settled one-time charge and the ownership it granted.
	purchase := func(o offer, txnPrefix string, age time.Duration) (openrails.PaymentID, openrails.CustomerID) {
		t.Helper()
		customer := openrails.CustomerID(uuid.New())
		_, err := remote.EnsureCustomer(ctx, customer.String())
		require.NoError(t, err)
		id := uuid.New()
		at := time.Now().UTC().Add(-age)
		exec(`INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,purchased_at,created_at)
			VALUES($1,$2,$3,$4,'nmi',$5,10000000,10000000,'USD','completed','rail',$6,$7,$7)`, id, owned.MerchantID.UUID(), customer.UUID(), sdkPriceID(t, o.price.ID).UUID(), txnPrefix+id.String(), psp, at)
		exec(`INSERT INTO billing.grants(merchant_id,customer_id,product_id,kind,source_type,source_id,payment_id,event,starts_at)
			VALUES($1,$2,$3,'ownership','purchase',$4,$5,'grant',$6)`, owned.MerchantID.UUID(), customer.UUID(), sdkProductID(t, o.product.ID).UUID(), id.String(), id, at)
		return openrails.PaymentID(id), customer
	}
	hasAccess := func(o offer, customer openrails.CustomerID) bool {
		t.Helper()
		check, err := remote.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: customer.String(), ProductID: o.product.ID})
		require.NoError(t, err)
		return check.HasAccess
	}

	for _, topology := range topologies {
		t.Run("refund payment "+topology.name, func(t *testing.T) {
			client := topology.client
			o := newOffer(remote)
			paid, customer := purchase(o, "ok-", time.Hour)
			key := uuid.NewString()
			partial := openrails.RefundPaymentParams{Amount: 4_000_000, Reason: "goodwill", IdempotencyKey: key}
			first, err := client.RefundPayment(ctx, paid, partial)
			require.NoError(t, err)
			require.Equal(t, "refund", first.Object)
			require.Equal(t, "succeeded", first.Status)
			require.EqualValues(t, -4_000_000, first.Amount)
			require.Equal(t, "goodwill", first.Reason)
			require.Equal(t, paid, *first.RefundedPaymentID)
			require.Equal(t, &openrails.ProductSummary{ID: o.product.ID, Key: o.product.Key, DisplayName: "Post"}, first.Product, "the refund names the product it reverses")
			require.True(t, hasAccess(o, customer), "a refund without revoke_access keeps the purchase's access")

			replay, err := client.RefundPayment(ctx, paid, partial)
			require.NoError(t, err)
			require.Equal(t, first.ID, replay.ID)
			require.Equal(t, 1, gateway.count("ok-"+paid.UUID().String()), "an idempotent retry never reaches the provider again")
			_, err = client.RefundPayment(ctx, paid, openrails.RefundPaymentParams{Amount: 5_000_000, IdempotencyKey: key})
			require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)

			full, err := client.RefundPayment(ctx, paid, openrails.RefundPaymentParams{Full: true, RevokeAccess: true, IdempotencyKey: uuid.NewString()})
			require.NoError(t, err)
			require.EqualValues(t, -6_000_000, full.Amount, "full refunds only the remaining amount")
			require.False(t, hasAccess(o, customer), "revoke_access ends the refunded purchase's access")
			got, err := client.GetPayment(ctx, paid)
			require.NoError(t, err)
			require.Equal(t, "refunded", got.Status)
			require.EqualValues(t, 10_000_000, got.AmountRefunded)
			require.Equal(t, first.Product, got.Product)
			for _, refund := range got.Refunds.Data {
				require.Equal(t, first.Product, refund.Product)
			}
			_, err = client.RefundPayment(ctx, paid, openrails.RefundPaymentParams{Full: true, IdempotencyKey: uuid.NewString()})
			require.ErrorIs(t, err, openrails.ErrInvalid)
			require.Equal(t, 2, gateway.count("ok-"+paid.UUID().String()))

			_, err = client.RefundPayment(ctx, paid, openrails.RefundPaymentParams{Amount: 1})
			require.ErrorIs(t, err, openrails.ErrInvalid, "the idempotency key is mandatory")
		})
	}

	t.Run("uncertain provider outcome reconciles without a second refund", func(t *testing.T) {
		o := newOffer(remote)
		paid, _ := purchase(o, "uncertain-", time.Hour)
		params := openrails.RefundPaymentParams{Full: true, IdempotencyKey: uuid.NewString()}
		pending, err := local.RefundPayment(ctx, paid, params)
		require.NoError(t, err)
		require.Equal(t, "pending", pending.Status)
		again, err := remote.RefundPayment(ctx, paid, params)
		require.NoError(t, err)
		require.Equal(t, pending.ID, again.ID)
		require.Equal(t, "pending", again.Status)
		_, err = remote.RefundPayment(ctx, paid, openrails.RefundPaymentParams{Full: true, IdempotencyKey: uuid.NewString()})
		require.ErrorIs(t, err, openrails.ErrInvalid, "the pending reservation holds the refundable amount")
		require.Equal(t, 1, gateway.count("uncertain-"+paid.UUID().String()), "an unknown outcome is never re-sent")
	})

	t.Run("rails without automatic refunds refuse with typed errors", func(t *testing.T) {
		other := surface.ProvisionOwnedMerchant("archive-unarmed-" + uuid.NewString()[:8])
		client := surface.Client(openrails.WithMerchantID(other.MerchantID), openrails.WithAPIKey(other.APIKey))
		otherPool := h.MerchantPool(other.MerchantID.UUID())
		product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "unarmed", DisplayName: "Unarmed"})
		require.NoError(t, err)
		price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
		require.NoError(t, err)
		for rail, want := range map[string]error{"nmi": openrails.ErrRefundRailUnavailable, "ccbill": openrails.ErrRefundUnsupported} {
			customer := openrails.CustomerID(uuid.New())
			_, err := client.EnsureCustomer(ctx, customer.String())
			require.NoError(t, err)
			id := uuid.New()
			_, err = otherPool.Exec(ctx, `INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id)
				VALUES($1,$2,$3,$4,$5,$6,10000000,10000000,'USD','completed','rail',$7)`, id, other.MerchantID.UUID(), customer.UUID(), sdkPriceID(t, price.ID).UUID(), rail, "unarmed-"+id.String(), dbtest.EnsureTestPSP(ctx, t, otherPool, other.MerchantID.UUID(), rail))
			require.NoError(t, err)
			_, err = client.RefundPayment(ctx, openrails.PaymentID(id), openrails.RefundPaymentParams{Full: true, IdempotencyKey: uuid.NewString()})
			require.ErrorIs(t, err, want, rail)
			var reservations int
			require.NoError(t, otherPool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE refunded_payment_id=$1`, id).Scan(&reservations))
			require.Zero(t, reservations, "a refused refund reserves nothing")
		}
	})

	t.Run("archive refunds purchases inside the window", func(t *testing.T) {
		o := newOffer(remote)
		old, oldCustomer := purchase(o, "ok-", 40*24*time.Hour)
		recent, recentCustomer := purchase(o, "ok-", 5*24*time.Hour)
		declined, declinedCustomer := purchase(o, "decline-", 24*time.Hour)
		refunded, _ := purchase(o, "ok-", 2*24*time.Hour)
		_, err := remote.RefundPayment(ctx, refunded, openrails.RefundPaymentParams{Full: true, IdempotencyKey: uuid.NewString()})
		require.NoError(t, err)
		before := gateway.total()

		params := openrails.ArchiveProductParams{ProductKey: o.product.Key, Action: openrails.PurchaseActionRefund, Window: 30 * 24 * time.Hour, Reason: "post deleted", IdempotencyKey: uuid.NewString()}
		archive, err := remote.ArchiveProduct(ctx, params)
		require.NoError(t, err)
		require.True(t, archive.Complete)
		require.Equal(t, o.product.ID, archive.ProductID.String())
		outcomes := map[openrails.PaymentID]openrails.ArchivedPurchase{}
		for _, item := range archive.Purchases {
			outcomes[item.PaymentID] = item
		}
		require.Len(t, outcomes, 3, "the purchase outside the window is untouched")
		require.NotContains(t, outcomes, old)
		require.Equal(t, openrails.ArchivedPurchaseRefunded, outcomes[recent].Outcome)
		require.NotNil(t, outcomes[recent].RefundID)
		require.Equal(t, openrails.ArchivedPurchaseReviewOpen, outcomes[declined].Outcome, "a refused refund falls back to review")
		require.Contains(t, outcomes[declined].Detail, "refund")
		require.Equal(t, openrails.ArchivedPurchaseAlreadyRefunded, outcomes[refunded].Outcome)
		require.Equal(t, before+2, gateway.total())

		product, err := remote.Products.Retrieve(ctx, o.product.ID)
		require.NoError(t, err)
		require.True(t, product.Archived, "the product is archived, never deleted")
		require.False(t, hasAccess(o, recentCustomer), "an archive refund ends that purchase's access")
		require.True(t, hasAccess(o, declinedCustomer), "a purchase under review keeps its access")
		require.True(t, hasAccess(o, oldCustomer))
		refund, err := remote.GetPayment(ctx, recent)
		require.NoError(t, err)
		require.Equal(t, "refunded", refund.Status)

		// Whole-operation replay, from the other topology, is a projection.
		replay, err := local.ArchiveProduct(ctx, params)
		require.NoError(t, err)
		require.Equal(t, archive.ID, replay.ID)
		require.Equal(t, archive.PurchasedSince, replay.PurchasedSince, "the window is fixed at first acceptance")
		require.ElementsMatch(t, archive.Purchases, replay.Purchases)
		require.Equal(t, before+2, gateway.total(), "a replay never refunds again")
		read, err := local.GetProductArchive(ctx, archive.ID)
		require.NoError(t, err)
		require.ElementsMatch(t, archive.Purchases, read.Purchases)

		changed := params
		changed.Action = openrails.PurchaseActionReview
		_, err = remote.ArchiveProduct(ctx, changed)
		require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
	})

	t.Run("archive records purchases for review", func(t *testing.T) {
		o := newOffer(remote)
		first, firstCustomer := purchase(o, "ok-", 3*24*time.Hour)
		second, secondCustomer := purchase(o, "ok-", 2*24*time.Hour)
		before := gateway.total()
		params := openrails.ArchiveProductParams{ProductID: o.product.ID, Action: openrails.PurchaseActionReview, PurchasedSince: time.Now().Add(-7 * 24 * time.Hour), IdempotencyKey: uuid.NewString()}
		archive, err := local.ArchiveProduct(ctx, params)
		require.NoError(t, err)
		require.Len(t, archive.Purchases, 2)
		for _, item := range archive.Purchases {
			require.Equal(t, openrails.ArchivedPurchaseReviewOpen, item.Outcome)
		}
		require.Equal(t, before, gateway.total(), "review mode moves no money")
		require.True(t, hasAccess(o, firstCustomer))

		reviews, err := remote.ListPurchaseReviews(ctx, openrails.PurchaseReviewFilter{ProductArchiveID: archive.ID})
		require.NoError(t, err)
		require.EqualValues(t, 2, reviews.Total)
		byPayment := map[openrails.PaymentID]openrails.PurchaseReview{}
		for _, review := range reviews.Data {
			require.Equal(t, openrails.PurchaseReviewOpen, review.Status)
			require.Equal(t, o.product.Key, review.ProductKey)
			require.EqualValues(t, 10_000_000, review.Amount)
			byPayment[review.PaymentID] = review
		}

		resolved, err := remote.ResolvePurchaseReview(ctx, byPayment[first].ID, openrails.ResolvePurchaseReviewParams{Decision: openrails.PurchaseReviewDecisionRefund})
		require.NoError(t, err)
		require.Equal(t, openrails.PurchaseReviewRefunded, resolved.Status)
		require.NotNil(t, resolved.RefundID)
		require.False(t, hasAccess(o, firstCustomer))
		again, err := local.ResolvePurchaseReview(ctx, byPayment[first].ID, openrails.ResolvePurchaseReviewParams{Decision: openrails.PurchaseReviewDecisionRefund})
		require.NoError(t, err)
		require.Equal(t, resolved.RefundID, again.RefundID)
		_, err = remote.ResolvePurchaseReview(ctx, byPayment[first].ID, openrails.ResolvePurchaseReviewParams{Decision: openrails.PurchaseReviewDecisionDismiss})
		require.ErrorIs(t, err, openrails.ErrConflict)
		dismissed, err := remote.ResolvePurchaseReview(ctx, byPayment[second].ID, openrails.ResolvePurchaseReviewParams{Decision: openrails.PurchaseReviewDecisionDismiss, Notes: "keep"})
		require.NoError(t, err)
		require.Equal(t, openrails.PurchaseReviewDismissed, dismissed.Status)
		require.True(t, hasAccess(o, secondCustomer))
		require.Equal(t, before+1, gateway.total())

		replay, err := remote.ArchiveProduct(ctx, params)
		require.NoError(t, err)
		for _, item := range replay.Purchases {
			want := map[openrails.PaymentID]string{first: openrails.ArchivedPurchaseReviewRefunded, second: openrails.ArchivedPurchaseReviewDismissed}[item.PaymentID]
			require.Equal(t, want, item.Outcome)
		}
		open, err := remote.ListPurchaseReviews(ctx, openrails.PurchaseReviewFilter{ProductArchiveID: archive.ID})
		require.NoError(t, err)
		require.Zero(t, open.Total, "a replay never reopens a resolved review")
	})

	t.Run("archive without a purchase policy", func(t *testing.T) {
		o := newOffer(remote)
		purchase(o, "ok-", time.Hour)
		archive, err := remote.ArchiveProduct(ctx, openrails.ArchiveProductParams{ProductKey: o.product.Key, IdempotencyKey: uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, openrails.PurchaseActionNone, archive.Action)
		require.Empty(t, archive.Purchases)
		product, err := local.Products.Retrieve(ctx, o.product.ID)
		require.NoError(t, err)
		require.True(t, product.Archived)
	})
}
