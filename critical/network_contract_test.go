//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestNetworkContracts(t *testing.T) {
	t.Run("OR-J02-S05/live-provider-agreement-refuses-unlink-and-terminal-cancel-releases-access", func(t *testing.T) {
		w := newStandaloneWorld(t)
		link := w.link(t, "alpha", "alice")
		var psp billing.PSP
		w.command(t, map[string]any{"Op": "declare-psp", "Merchant": w.Merchants["alpha"].ID.String(), "Body": billing.PSPDeclaration{Key: "historical", Rail: billing.RailCCBill, Environment: "sandbox", PSPAccount: billing.PSPAccount{ClientAccnum: "123456", ClientSubacc: "0000"}}}, &psp)
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "linked-member", DisplayName: "Linked member"})
		require.NoError(t, err)
		price, err := w.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "legacy-weekly", PSP: "historical", FlexID: "critical-flex", RecurringBillingOptionID: "123", UnitAmount: ptr(int64(10_000_000)), Currency: "USD", BillingIntervalHours: ptr(168)})
		require.NoError(t, err)
		started := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
		due := started.Add(7 * 24 * time.Hour)
		book := billing.DeclaredBilling{AsOf: started, DefaultPSP: billing.PSPRef{ID: &psp.ID}, Customers: []billing.DeclaredCustomer{{Customer: link.CustomerID}}, Subscriptions: []billing.DeclaredSubscription{{SourceID: "linked-agreement", Customer: link.CustomerID, Price: price.ID, Rail: "ccbill", RailSubscriptionID: "123456789", StartedAt: started, PaidThrough: &due, AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: "123456789", Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: due}}}, Transactions: []billing.DeclaredTransaction{{RailSubscriptionID: "123456789", TransactionID: "987654321", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: started, NextBillingAt: &due}}}
		imported, err := w.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
		require.Contains(t, imported.Imported, "linked-agreement")
		subs, err := w.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: link.CustomerID})
		require.NoError(t, err)
		require.Len(t, subs.Items, 1)
		require.Equal(t, billing.SubscriptionActive, subs.Items[0].Status)
		status, raw := w.request(t, http.MethodDelete, "/v1/network/links/"+link.ID.String(), w.AliceToken, nil)
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), billing.CodeNetworkLinkInUse)
		status, raw = w.request(t, http.MethodGet, "/v1/network/links", w.AliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), link.ID.String())
		canceled, err := w.alpha.CancelSubscription(t.Context(), subs.Items[0].ID, billing.CancelSubscriptionParams{AccountDeletion: true, Reason: "terminal imported customer closure"})
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
		status, raw = w.request(t, http.MethodDelete, "/v1/network/links/"+link.ID.String(), w.AliceToken, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		status, raw = w.request(t, http.MethodGet, "/v1/network/links/"+link.ID.String()+"/subscriptions/"+canceled.ID.String(), w.AliceToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		retained, err := w.alpha.GetSubscription(t.Context(), canceled.ID)
		require.NoError(t, err)
		require.Equal(t, link.CustomerID, retained.CustomerID)
		payments, err := w.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: link.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1)
		require.EqualValues(t, 10_000_000, payments.Items[0].Amount)
	})
	t.Run("OR-J02-S03/network-alias-is-only-the-linked-owned-customer", func(t *testing.T) {
		w := newStandaloneWorld(t)
		alpha := w.link(t, "alpha", "alice")
		beta := w.link(t, "beta", "bob")
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "network-order", DisplayName: "Network order"})
		require.NoError(t, err)
		price, err := w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
		require.NoError(t, err)
		order, err := w.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: w.Merchants["alpha"].CustomerID, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: "owned-network-order"})
		require.NoError(t, err)
		path := "/v1/network/links/" + alpha.ID.String() + "/orders/" + order.ID.String()
		status, raw := w.request(t, http.MethodGet, path, w.AliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var read billing.Order
		require.NoError(t, json.Unmarshal(raw, &read))
		require.Equal(t, order.ID, read.ID)
		require.Equal(t, alpha.CustomerID, read.CustomerID)
		status, raw = w.request(t, http.MethodGet, path, w.BobToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.NotContains(t, string(raw), alpha.CustomerID.String())
		status, raw = w.request(t, http.MethodGet, "/v1/network/links/"+beta.ID.String()+"/orders/"+order.ID.String(), w.AliceToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		status, raw = w.request(t, http.MethodGet, "/v1/network/links", w.Merchants["alpha"].CustomerToken, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		status, raw = w.request(t, http.MethodGet, "/v1/network/orders", w.AliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var page billing.ListPage[billing.NetworkOrder]
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Len(t, page.Items, 1)
		require.Equal(t, alpha.ID, page.Items[0].LinkID)
		require.Equal(t, order.ID, page.Items[0].Order.ID)
	})
	t.Run("OR-J02-S04/stale-real-session-cannot-unlink-until-fresh-sign-in", func(t *testing.T) {
		w := newStandaloneWorld(t)
		link := w.link(t, "alpha", "alice")
		var token struct {
			AccessToken string `json:"access_token"`
		}
		w.command(t, map[string]string{"Op": "stale-sign-in", "Account": "alice"}, &token)
		status, raw := w.request(t, http.MethodGet, "/v1/network/links", token.AccessToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), link.ID.String())
		status, raw = w.request(t, http.MethodDelete, "/v1/network/links/"+link.ID.String(), token.AccessToken, nil)
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
		require.Contains(t, string(raw), "step_up_required")
		status, raw = w.request(t, http.MethodGet, "/v1/network/links", w.AliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), link.ID.String())
		status, raw = w.request(t, http.MethodDelete, "/v1/network/links/"+link.ID.String(), w.AliceToken, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		status, raw = w.request(t, http.MethodGet, "/v1/network/links", w.AliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NotContains(t, string(raw), link.ID.String())
	})

	t.Run("OR-J02-S02/foreign-nonce-customer-and-expired-challenges-change-no-link", func(t *testing.T) {
		require.NotEmpty(t, os.Getenv("OPENRAILS_CRITICAL_REDIS_URL"), "selected challenge expiry needs a real owned Redis")
		w := newStandaloneWorld(t)
		options, err := redis.ParseURL(os.Getenv("OPENRAILS_CRITICAL_REDIS_URL"))
		require.NoError(t, err)
		rdb := redis.NewClient(options)
		defer rdb.Close()
		for _, profile := range []string{"wrong-merchant", "wrong-nonce", "expired"} {
			t.Run(profile, func(t *testing.T) {
				challenge := w.challenge(t, "alpha")
				nonce := challenge.Nonce
				if profile == "wrong-nonce" {
					nonce = "another-valid-OAuth-nonce"
				}
				proof := w.proof(t, "alpha", "alice", nonce)
				credential := w.Merchants["alpha"].CustomerToken
				if profile == "wrong-merchant" {
					credential = w.Merchants["beta"].CustomerToken
				}
				if profile == "expired" {
					// Expire only this fixture's real Redis TTL. The public
					// ExpiresAt separately proves the declared five-minute life;
					// no five-minute CI sleep or fake challenge verifier is used.
					key := "network-link:" + challenge.ID
					ttl, err := rdb.PTTL(t.Context(), key).Result()
					require.NoError(t, err)
					require.Greater(t, ttl, time.Minute)
					require.LessOrEqual(t, ttl, 5*time.Minute)
					expired, err := rdb.PExpire(t.Context(), key, 0).Result()
					require.NoError(t, err)
					require.True(t, expired)
				}
				status, raw := w.request(t, http.MethodPost, "/v1/me/network-link", credential, billing.CreateNetworkLinkParams{ChallengeID: challenge.ID, IDToken: proof})
				require.Equal(t, http.StatusBadRequest, status, string(raw))
				require.Contains(t, string(raw), billing.CodeNetworkLinkChallengeInvalid)
			})
		}
		events, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"customer.network_linked"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, events.Events)
		challenge := w.challenge(t, "alpha")
		proof := w.proof(t, "alpha", "alice", challenge.Nonce)
		status, raw := w.request(t, http.MethodPost, "/v1/me/network-link", w.Merchants["alpha"].CustomerToken, billing.CreateNetworkLinkParams{ChallengeID: challenge.ID, IDToken: proof})
		require.Equal(t, http.StatusCreated, status, string(raw))
	})

	t.Run("OR-J02-S01/both-real-identities-link-through-consented-one-use-proof", func(t *testing.T) {
		w := newStandaloneWorld(t)
		before := time.Now()
		challenge := w.challenge(t, "alpha")
		require.WithinDuration(t, before.Add(5*time.Minute), challenge.ExpiresAt, 5*time.Second)
		proof := w.proof(t, "alpha", "alice", challenge.Nonce)
		body := billing.CreateNetworkLinkParams{ChallengeID: challenge.ID, IDToken: proof}
		status, raw := w.request(t, http.MethodPost, "/v1/me/network-link", w.Merchants["alpha"].CustomerToken, body)
		require.Equal(t, http.StatusCreated, status, string(raw))
		var link billing.CustomerLink
		require.NoError(t, json.Unmarshal(raw, &link))
		require.Equal(t, w.Merchants["alpha"].ID, link.MerchantID)
		require.Equal(t, w.Merchants["alpha"].CustomerID, link.CustomerID)
		require.Equal(t, w.AliceID, link.AccountID.String())
		require.Equal(t, billing.CustomerLinkConnect, link.Method)
		require.Equal(t, billing.CustomerLinkActive, link.Status)
		status, raw = w.request(t, http.MethodPost, "/v1/me/network-link", w.Merchants["alpha"].CustomerToken, body)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), billing.CodeNetworkLinkChallengeInvalid)
		account := billing.NetworkAccountID(uuid.MustParse(w.AliceID))
		own, err := w.alpha.ListNetworkCustomers(t.Context(), billing.NetworkCustomerListParams{NetworkAccountID: account})
		require.NoError(t, err)
		require.Len(t, own.Items, 1)
		require.Equal(t, link.CustomerID, own.Items[0].ID)
		foreign, err := w.beta.ListNetworkCustomers(t.Context(), billing.NetworkCustomerListParams{NetworkAccountID: account})
		require.NoError(t, err)
		require.Empty(t, foreign.Items)
		next := w.challenge(t, "alpha")
		token := w.proof(t, "alpha", "alice", next.Nonce)
		status, raw = w.request(t, http.MethodPost, "/v1/me/network-link", w.Merchants["alpha"].CustomerToken, billing.CreateNetworkLinkParams{ChallengeID: next.ID, IDToken: token})
		require.Equal(t, http.StatusOK, status, string(raw))
		var repeated billing.CustomerLink
		require.NoError(t, json.Unmarshal(raw, &repeated))
		require.Equal(t, link.ID, repeated.ID)
		events, err := w.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"customer.network_linked"}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events.Events, 1)
		// A native network account keeps its own customer identity. An
		// asserted merchant user merely sharing issuer NULL is not native.
		var native struct {
			AccessToken string `json:"access_token"`
		}
		w.command(t, map[string]any{"Op": "authorize", "Merchant": "beta", "Account": "alice", "Scopes": []string{"openid", "openrails:self"}}, &native)
		status, raw = w.request(t, http.MethodPost, "/v1/me/network-link/challenges", native.AccessToken, map[string]any{})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var nativeChallenge billing.NetworkLinkChallenge
		require.NoError(t, json.Unmarshal(raw, &nativeChallenge))
		foreignProof := w.proof(t, "beta", "bob", nativeChallenge.Nonce)
		status, raw = w.request(t, http.MethodPost, "/v1/me/network-link", native.AccessToken, billing.CreateNetworkLinkParams{ChallengeID: nativeChallenge.ID, IDToken: foreignProof})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), billing.CodeNetworkLinkConflict)
		status, raw = w.request(t, http.MethodPost, "/v1/me/network-link/challenges", native.AccessToken, map[string]any{})
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &nativeChallenge))
		sameProof := w.proof(t, "beta", "alice", nativeChallenge.Nonce)
		status, raw = w.request(t, http.MethodPost, "/v1/me/network-link", native.AccessToken, billing.CreateNetworkLinkParams{ChallengeID: nativeChallenge.ID, IDToken: sameProof})
		require.Equal(t, http.StatusOK, status, string(raw))
		var same billing.CustomerLink
		require.NoError(t, json.Unmarshal(raw, &same))
		require.Equal(t, w.AliceID, same.AccountID.String())
		require.Equal(t, w.AliceID, same.CustomerID.String())
		var deleted struct {
			DeletedAt *time.Time `json:"deleted_at"`
		}
		w.command(t, map[string]any{"Op": "soft-delete", "ID": w.BobID}, &deleted)
		require.NotNil(t, deleted.DeletedAt)
		var asserted struct{ AccessToken string }
		w.command(t, map[string]any{"Op": "customer-token", "Merchant": "beta", "ID": w.BobID}, &asserted)
		status, raw = w.request(t, http.MethodPost, "/v1/me/network-link/challenges", asserted.AccessToken, map[string]any{})
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &nativeChallenge))
		otherProof := w.proof(t, "beta", "alice", nativeChallenge.Nonce)
		status, raw = w.request(t, http.MethodPost, "/v1/me/network-link", asserted.AccessToken, billing.CreateNetworkLinkParams{ChallengeID: nativeChallenge.ID, IDToken: otherProof})
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), billing.CodeNetworkLinkConflict)

	})
}
