//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestNetworkContracts(t *testing.T) {
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
