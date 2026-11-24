package ccbill_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
)

// statusReads answers each subscription-management call with the next
// answer, or holds it until release when the answer is "hold".
type statusReads struct {
	mu      sync.Mutex
	answers []string
	calls   int
	arrived chan struct{}
	release chan struct{}
}

func (s *statusReads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls++
	answer := "<results>-3</results>"
	if len(s.answers) > 0 {
		answer, s.answers = s.answers[0], s.answers[1:]
	}
	s.mu.Unlock()
	switch answer {
	case "hold":
		s.arrived <- struct{}{}
		<-s.release
		answer = "<results>-3</results>"
	case "503":
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, answer)
}

func (s *statusReads) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func probeWorld(t *testing.T, answers ...string) (*statusReads, func(username string) *ccbill.DataLinkClient, *time.Time) {
	t.Helper()
	reads := &statusReads{answers: answers, arrived: make(chan struct{}, 1), release: make(chan struct{})}
	server := httptest.NewServer(reads)
	t.Cleanup(server.Close)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	shared := &slots{next: map[string]time.Time{}}
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	client := func(username string) *ccbill.DataLinkClient {
		c := ccbill.NewDataLinkClient(slog.New(slog.DiscardHandler), &config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0001", DataLinkUsername: username, DataLinkPassword: "p"})
		c.BaseURL, c.Pacing = server.URL, ccbill.Pacing{Slots: shared, Now: clock}
		return c
	}
	return reads, client, &now
}

// CCBill locks a login after three refused logins in an hour: two refused
// probes hold that login, and only it.
func TestProbeHoldsALoginAfterTwoRefusals(t *testing.T) {
	reads, client, _ := probeWorld(t, "<results>-1</results>", "<results>-1</results>")
	stranger := client("stranger")
	for range 2 {
		if err := stranger.ProbeCredentials(t.Context()); !errors.Is(err, ccbill.ErrDataLinkAuth) {
			t.Fatalf("a refused login probed = %v", err)
		}
	}
	var limited *ccbill.ProbeLimitedError
	if err := stranger.ProbeCredentials(t.Context()); !errors.As(err, &limited) || limited.Wait != ccbill.ProbeInterval {
		t.Fatalf("a third probe of a twice refused login = %v", err)
	}
	if reads.count() != 2 {
		t.Fatalf("the held probe reached CCBill: %d calls", reads.count())
	}
	if err := client("owner").ProbeCredentials(t.Context()); err != nil {
		t.Fatalf("another login of the account was held: %v", err)
	}
}

// No answer, a 5xx and a -7 are no refusal: they count nothing.
func TestProbeCountsOnlyRefusals(t *testing.T) {
	_, client, _ := probeWorld(t, "<results>-7</results>", "503", "<results>-1</results>", "<results>-1</results>")
	owner := client("owner")
	if err := owner.ProbeCredentials(t.Context()); !errors.Is(err, ccbill.ErrDataLinkIndeterminate) {
		t.Fatalf("a -7 = %v", err)
	}
	if err := owner.ProbeCredentials(t.Context()); err == nil || errors.Is(err, ccbill.ErrDataLinkAuth) {
		t.Fatalf("a 503 = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := owner.ProbeCredentials(ctx); err == nil {
		t.Fatal("a canceled probe succeeded")
	}
	for range 2 {
		if err := owner.ProbeCredentials(t.Context()); !errors.Is(err, ccbill.ErrDataLinkAuth) {
			t.Fatalf("after answers that count nothing, a refusal = %v", err)
		}
	}
}

// A probe in flight leases its login for ProbeTimeout: another waits that
// long at most, and one killed in flight holds it no longer.
func TestProbeInFlightLeasesItsLogin(t *testing.T) {
	reads, client, now := probeWorld(t, "hold")
	done := make(chan error, 1)
	go func() { done <- client("owner").ProbeCredentials(context.Background()) }()
	<-reads.arrived
	var limited *ccbill.ProbeLimitedError
	if err := client("owner").ProbeCredentials(t.Context()); !errors.As(err, &limited) || limited.Wait != ccbill.ProbeTimeout {
		t.Fatalf("a probe beside one in flight = %v", err)
	}
	// The first never returns in time, as a killed one never does.
	*now = now.Add(ccbill.ProbeTimeout)
	if err := client("owner").ProbeCredentials(t.Context()); err != nil {
		t.Fatalf("a lease outlived its timeout: %v", err)
	}
	close(reads.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
