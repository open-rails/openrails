package ccbill_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
)

type slots struct {
	mu   sync.Mutex
	next map[string]time.Time
}

func (s *slots) Claim(_ context.Context, key string, now, next time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held := s.next[key]; held.After(now) {
		return held, false
	}
	s.next[key] = next
	return next, true
}

func (s *slots) Reopen(_ context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.next, key)
}

func TestLiveDataLinkTakesOneRequestAnHour(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	dataLink := ccbillmock.New()
	t.Cleanup(dataLink.Close)
	dataLink.SetClock(clock)
	client := func(sandbox bool) *ccbill.DataLinkClient {
		c := ccbill.NewDataLinkClient(slog.New(slog.DiscardHandler), &config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0001", DataLinkUsername: "u", DataLinkPassword: "p", Sandbox: sandbox})
		c.BaseURL, c.Pacing = dataLink.URL(), ccbill.Pacing{Slots: &slots{next: map[string]time.Time{}}, Now: clock}
		return c
	}
	live := client(false)
	if _, err := live.FetchActiveMembers(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := live.FetchTransactionExport(t.Context(), now.Add(-time.Hour), now, ccbill.AllDataLinkTxnTypes)
	if wait, paced := ccbill.PacedFor(err); !paced || wait != time.Hour || !errors.Is(err, ccbill.ErrDataLinkPaced) {
		t.Fatalf("a second request in the hour = %v (%v)", err, wait)
	}
	if _, err := live.FetchTransactionExport(t.Context(), now.Add(-48*time.Hour), now, ccbill.AllDataLinkTxnTypes); err == nil {
		t.Fatal("a live export of two days was asked")
	}
	// Another process, its own slots: CCBill refuses it, and it waits.
	_, err = client(false).FetchActiveMembers(t.Context())
	if wait, paced := ccbill.PacedFor(err); !paced || wait != time.Hour {
		t.Fatalf("CCBill's refusal = %v (%v)", err, wait)
	}
	if _, err := client(true).FetchActiveMembers(t.Context()); err != nil {
		t.Fatalf("test mode is not paced: %v", err)
	}
	// Proving credentials is a status read, no export: no slot taken, none
	// refused, and a refused login is told apart from a missing record.
	exports := len(dataLink.Requests())
	if err := live.ProbeCredentials(t.Context()); err != nil {
		t.Fatalf("a credential probe inside the hour: %v", err)
	}
	dataLink.RequireCredentials("u", "p", false)
	stranger := client(false)
	stranger.Password = "wrong"
	if err := stranger.ProbeCredentials(t.Context()); !errors.Is(err, ccbill.ErrDataLinkAuth) {
		t.Fatalf("wrong credentials probed = %v", err)
	}
	if got := len(dataLink.Requests()); got != exports {
		t.Fatalf("the probes made %d DataLink exports", got-exports)
	}
	if calls := dataLink.Calls(); len(calls) != 2 || calls[0].Form.Get("action") != "viewSubscriptionStatus" {
		t.Fatalf("the probes were %v", calls)
	}
	// A failed probe holds the account's probes an hour, sent to CCBill or
	// not; a proven one held nothing.
	var limited *ccbill.ProbeLimitedError
	if err := stranger.ProbeCredentials(t.Context()); !errors.As(err, &limited) || limited.Wait != ccbill.ProbeInterval {
		t.Fatalf("a probe after a failed one = %v", err)
	}
	if err := live.ProbeCredentials(t.Context()); err != nil {
		t.Fatalf("a proven login's slot was kept: %v", err)
	}
	if calls := dataLink.Calls(); len(calls) != 3 {
		t.Fatalf("the held probe reached CCBill: %v", calls)
	}
	now = now.Add(time.Hour)
	if _, err := live.FetchTransactionExport(t.Context(), now.Add(-time.Hour), now, ccbill.AllDataLinkTxnTypes); err != nil {
		t.Fatalf("the next hour's request: %v", err)
	}
	if got := dataLink.Unexpected(); len(got) != 1 {
		t.Fatalf("CCBill saw %v; want the one request it refused", got)
	}
}
