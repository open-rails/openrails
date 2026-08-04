package nmi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stalledServer never answers. Requests block until the caller's context (or
// the per-request deadline) tears the connection down. Closing is deferred to
// t.Cleanup AFTER unblocking, because httptest.Server.Close waits for handlers.
func stalledServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(2 * time.Minute):
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

// or#866: before ctx plumbing the NMI package built every request with a bare
// http.NewRequest, so a cancelled job context could NOT abort an in-flight
// call — the caller sat for the full client timeout (25s), and
// ProviderRefreshWorker fans out many such calls. tests/ teardown hung in
// river.Client.Stop for exactly this reason.
//
// Each case must return in well under the client's own bound.
func TestStalledGatewayHonorsCallerContext(t *testing.T) {
	srv := stalledServer(t)
	client := testClient(t, srv.URL)

	cases := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"v5 read (GetSubscription)", func(ctx context.Context) error {
			_, _, err := client.GetSubscription(ctx, "sub_1")
			return err
		}},
		{"v5 roster page (ListCustomersPage)", func(ctx context.Context) error {
			_, err := client.ListCustomersPage(ctx, "", 10, "")
			return err
		}},
		{"v5 mutation (RunSale)", func(ctx context.Context) error {
			_, err := client.RunSale(ctx, SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", OrderID: "o1"})
			return err
		}},
		{"classic direct-post (AddRecurringSubscription)", func(ctx context.Context) error {
			_, err := client.AddRecurringSubscription(ctx, RecurringPaymentData{PlanID: "p", CustomerVaultID: "v1", Currency: "USD"})
			return err
		}},
		{"classic query (SearchTransactions)", func(ctx context.Context) error {
			_, err := client.SearchTransactions(ctx, QueryFilter{OrderID: "o1"})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- tc.call(ctx) }()

			// Cancel like a shutting-down worker would.
			time.AfterFunc(100*time.Millisecond, cancel)
			defer cancel()

			select {
			case err := <-done:
				if elapsed := time.Since(start); elapsed > 5*time.Second {
					t.Fatalf("returned after %s: the caller's cancel did not abort the in-flight request", elapsed)
				}
				if err == nil {
					t.Fatal("expected an error from a stalled gateway")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("want context.Canceled in the chain, got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("call did not return within 5s of the caller's cancel — context is not reaching the request")
			}
		})
	}
}

// A mutation aborted mid-flight is an UNKNOWN outcome, not a decline: the
// request may already have executed at the gateway. Cancelling must not
// downgrade that classification (#674).
func TestCancelledMutationStaysAmbiguous(t *testing.T) {
	srv := stalledServer(t)
	client := testClient(t, srv.URL)

	for _, tc := range []struct {
		name string
		call func(context.Context) error
	}{
		{"v5 sale", func(ctx context.Context) error {
			_, err := client.RunSale(ctx, SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", OrderID: "o1"})
			return err
		}},
		{"classic direct-post", func(ctx context.Context) error {
			_, err := client.AddRecurringSubscription(ctx, RecurringPaymentData{PlanID: "p", CustomerVaultID: "v1", Currency: "USD"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := tc.call(ctx)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !IsTransportAmbiguous(err) {
				t.Fatalf("a cancelled mutation must stay transport-ambiguous (verify, never decline); got %v", err)
			}
		})
	}
}

// Reads get the tighter bound; mutations keep the generous one. Asserted on the
// request the choke point actually builds, so a future path that skips
// newRequest cannot quietly inherit the wrong deadline.
func TestPerRequestDeadlineBounds(t *testing.T) {
	client := testClient(t, "http://127.0.0.1:1")

	for _, tc := range []struct {
		name     string
		mutating bool
		want     time.Duration
	}{
		{"read", false, nmiReadTimeout},
		{"mutation", true, nmiMutationTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, cancel, err := client.newRequest(context.Background(), http.MethodGet, "http://example.invalid/x", nil, tc.mutating)
			if err != nil {
				t.Fatalf("newRequest: %v", err)
			}
			defer cancel()
			deadline, ok := req.Context().Deadline()
			if !ok {
				t.Fatal("request carries no deadline")
			}
			if got := time.Until(deadline); got > tc.want || got < tc.want-time.Second {
				t.Fatalf("deadline %s, want ~%s", got, tc.want)
			}
		})
	}

	t.Run("caller deadline wins when shorter", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, rcancel, err := client.newRequest(ctx, http.MethodPost, "http://example.invalid/x", nil, true)
		if err != nil {
			t.Fatalf("newRequest: %v", err)
		}
		defer rcancel()
		deadline, _ := req.Context().Deadline()
		if got := time.Until(deadline); got > 2*time.Second {
			t.Fatalf("caller's 2s deadline was widened to %s by the %s mutation bound", got, nmiMutationTimeout)
		}
	})
}
