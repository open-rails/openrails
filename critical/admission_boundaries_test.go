//go:build integration

package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func fundedCustomer(t *testing.T, w *world) (billing.CustomerID, billing.CreditGrant) {
	t.Helper()
	customer := billing.CustomerID(uuid.New())
	grants, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "funded"}})
	require.NoError(t, err)
	require.Len(t, grants, 1)
	return customer, grants[0]
}
func reservation(w *world, customer billing.CustomerID, id string, amount int64) billing.AdmitParams {
	return billing.AdmitParams{RequestID: id, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: amount, ExpiresAt: ptr(w.clock.Now().Add(time.Hour))}
}
func capture(id string, amount int64) billing.CaptureAdmissionParams {
	return billing.CaptureAdmissionParams{RequestID: id, Amount: ptr(amount), Usage: billing.CaptureUsage{EventType: "compute", Dimensions: map[string]int64{"units": 1}}}
}

func TestAdmissionBoundaries(t *testing.T) {
	t.Run("OR-J23-S01/batch-results-and-hold-lifecycle", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer, _ := fundedCustomer(t, w)
		items := []billing.AdmitParams{reservation(w, customer, "capture", 300), reservation(w, customer, "release", 200), reservation(w, billing.CustomerID{}, "invalid", 100)}
		results, err := w.alpha.Admit(t.Context(), items)
		require.NoError(t, err)
		require.Len(t, results, 3)
		require.True(t, results[0].Allowed())
		require.True(t, results[1].Allowed())
		require.False(t, results[2].Allowed())
		require.NotNil(t, results[2].Error)
		balance(t, w.alpha, customer, 500, 500, 1000)
		deadline := w.clock.Now().Add(2 * time.Hour)
		extended, err := w.alpha.ExtendAdmissions(t.Context(), []billing.ExtendAdmissionParams{{RequestID: "capture", ExpiresAt: deadline}})
		require.NoError(t, err)
		require.Nil(t, extended[0].Error)
		require.WithinDuration(t, deadline, *extended[0].Admission.ExpiresAt, time.Microsecond)
		settled, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("capture", 100), capture("missing", 100)})
		require.NoError(t, err)
		require.NoError(t, settled[0].Err())
		require.Error(t, settled[1].Err())
		require.EqualValues(t, 100, settled[0].Receipt.Amount)
		balance(t, w.alpha, customer, 700, 200, 900)
		released, err := w.alpha.ReleaseAdmissions(t.Context(), []string{"release"})
		require.NoError(t, err)
		require.Nil(t, released[0].Error)
		require.Equal(t, billing.AdmissionReleased, *released[0].Admission.State)
		balance(t, w.alpha, customer, 900, 0, 900)
		replay, err := w.alpha.ReleaseAdmissions(t.Context(), []string{"release"})
		require.NoError(t, err)
		require.Nil(t, replay[0].Error)
		require.Equal(t, billing.AdmissionReleased, *replay[0].Admission.State)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 2)
	})
	t.Run("OR-J23-S02/capture-release-race-has-one-terminal-outcome", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer, _ := fundedCustomer(t, w)
		admitted, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "race", 600)})
		require.NoError(t, err)
		require.True(t, admitted[0].Allowed())
		type captureResult struct {
			rows []billing.CaptureResult
			err  error
		}
		type releaseResult struct {
			rows []billing.AdmissionResult
			err  error
		}
		start := make(chan struct{})
		captured := make(chan captureResult, 1)
		released := make(chan releaseResult, 1)
		go func() {
			<-start
			rows, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("race", 450)})
			captured <- captureResult{rows, err}
		}()
		go func() {
			<-start
			rows, err := w.alpha.ReleaseAdmissions(t.Context(), []string{"race"})
			released <- releaseResult{rows, err}
		}()
		close(start)
		c, r := <-captured, <-released
		require.NoError(t, c.err)
		require.NoError(t, r.err)
		require.Len(t, c.rows, 1)
		require.Len(t, r.rows, 1)
		final, err := w.alpha.GetAdmission(t.Context(), "race")
		require.NoError(t, err)
		require.NotNil(t, final.State)
		switch *final.State {
		case billing.AdmissionCaptured:
			require.NoError(t, c.rows[0].Err())
			require.EqualValues(t, 450, c.rows[0].Receipt.Amount)
			balance(t, w.alpha, customer, 550, 0, 550)
		case billing.AdmissionReleased:
			require.Error(t, c.rows[0].Err())
			balance(t, w.alpha, customer, 1000, 0, 1000)
		default:
			t.Fatalf("reservation ended in unexpected state %s", *final.State)
		}
	})
	t.Run("OR-J23-S03/request-identity-refuses-changed-terms", func(t *testing.T) {
		w := newWorld(t)
		customer, _ := fundedCustomer(t, w)
		request := reservation(w, customer, "stable-reservation", 600)
		first, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{request})
		require.NoError(t, err)
		require.True(t, first[0].Allowed())
		same, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{request})
		require.NoError(t, err)
		require.True(t, same[0].Admission.Replayed)
		request.EstimatedAmount = 601
		changed, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{request})
		require.NoError(t, err)
		require.Equal(t, 422, changed[0].Status)
		require.Equal(t, "idempotency_key_reused", changed[0].Error.Code)
		balance(t, w.alpha, customer, 400, 600, 1000)
	})
	t.Run("OR-J22-S02/capture-and-credit-revocation-conserve-funds", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		customer, grant := fundedCustomer(t, w)
		admitted, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, "held-credit", 600)})
		require.NoError(t, err)
		require.True(t, admitted[0].Allowed())
		reason := billing.RevokeCreditGrantParams{Reason: "Revoke only unused credit"}
		_, err = w.alpha.RevokeCreditGrant(t.Context(), grant.ID, reason)
		var held *billing.StatusError
		require.ErrorAs(t, err, &held)
		require.Equal(t, "credit_grant_held", held.Code)
		type captureResult struct {
			rows []billing.CaptureResult
			err  error
		}
		type revokeResult struct {
			grant *billing.CreditGrant
			err   error
		}
		start := make(chan struct{})
		cch := make(chan captureResult, 1)
		rch := make(chan revokeResult, 1)
		go func() {
			<-start
			rows, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture("held-credit", 450)})
			cch <- captureResult{rows, err}
		}()
		go func() {
			<-start
			result, err := w.alpha.RevokeCreditGrant(t.Context(), grant.ID, reason)
			rch <- revokeResult{result, err}
		}()
		close(start)
		c, r := <-cch, <-rch
		require.NoError(t, c.err)
		require.NoError(t, c.rows[0].Err())
		if r.err != nil {
			var refusal *billing.StatusError
			require.ErrorAs(t, r.err, &refusal)
			require.Equal(t, "credit_grant_held", refusal.Code)
		}
		final, err := w.alpha.GetCreditGrant(t.Context(), grant.ID)
		require.NoError(t, err)
		require.EqualValues(t, 450, final.SpentAmount)
		require.GreaterOrEqual(t, final.RemainingAmount, int64(0))
		require.GreaterOrEqual(t, final.RevokedAmount, int64(0))
		require.EqualValues(t, 1000, final.SpentAmount+final.RemainingAmount+final.RevokedAmount+final.ExpiredAmount)
		if r.err == nil {
			require.EqualValues(t, 550, final.RevokedAmount)
			balance(t, w.alpha, customer, 0, 0, 0)
		} else {
			require.EqualValues(t, 550, final.RemainingAmount)
			balance(t, w.alpha, customer, 550, 0, 550)
		}
	})
}
