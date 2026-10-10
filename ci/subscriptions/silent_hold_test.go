//go:build e2e && integration

package subscriptions_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// An admission hold stops reserving the customer's money at its deadline on
// its own. A provider operation's hold never expires: one its host stopped
// driving keeps reserving the customer's money, so the convergence sweep
// raises a finding for it, and the finding clears once the host acts.
func TestSilentProviderOperationRaisesFinding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	c := w.newCustomer()
	client := w.client[embedded]
	_, err := client.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{
		{CustomerID: c.cid(), Currency: "USD", Amount: 10_000_000, Source: "support", SourceID: "seed"},
	})
	require.NoError(t, err)
	held := func() int64 {
		t.Helper()
		bal, err := client.GetBalance(ctx, c.cid(), "USD")
		require.NoError(t, err)
		return bal.HeldAmount
	}

	expires := w.clock.Now().Add(time.Hour)
	verdicts, err := client.Admit(ctx, []billing.AdmitParams{{RequestID: uuid.NewString(), CustomerID: c.cid(), Invoker: c.id, InvokerType: billing.InvokerTypeCustomer,
		Currency: "USD", EstimatedAmount: 2_000_000, ExpiresAt: &expires}})
	require.NoError(t, err)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
	require.EqualValues(t, 2_000_000, held())
	w.advance(2 * time.Hour)
	require.Zero(t, held(), "an admission hold past its deadline reserves nothing")

	body := []byte(`{"rental":"silent"}`)
	digest := sha256.Sum256(body)
	status, opened := w.hostJSON(http.MethodPost, "/v1/admin/provider-operations", map[string]any{
		"operation_id": "rental-silent", "customer_id": c.id, "record_owner": "user:1", "currency": "USD", "amount": "1000000",
		"claim_reference": "claim:silent", "authorization_body": base64.StdEncoding.EncodeToString(body),
		"authorization_body_sha256": hex.EncodeToString(digest[:]),
	})
	require.Equal(t, http.StatusCreated, status, "%v", opened)
	findings := func() []string {
		t.Helper()
		w.converge()
		return w.openFindings("life.provider_operation.silent")
	}
	require.Empty(t, findings(), "a hold its host is driving is not silent")

	w.advance(8 * 24 * time.Hour)
	require.EqualValues(t, 1_000_000, held(), "the operation still holds the customer's money")
	require.Equal(t, []string{"provider_operation:rental-silent"}, findings())

	status, released := w.hostJSON(http.MethodPost, "/v1/admin/provider-operations/rental-silent/release", map[string]any{"release_reference": "never-created"})
	require.Equal(t, http.StatusOK, status, "%v", released)
	require.Zero(t, held())
	require.Empty(t, findings(), "the finding clears with the release")
}
