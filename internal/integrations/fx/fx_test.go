package fx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exact rational conversion with one final ceiling, across ISO scales.
func TestConvertAmount(t *testing.T) {
	p := NewMockProvider(map[string]float64{"eur": 1.08, "jpy": 0.01})
	for _, tc := range []struct {
		from, to  string
		amt, want int64
	}{
		{"EUR", "USD", 100, 108},           // float 100*1.08 = 108.00000000000001 would ceil to 109
		{"USD", "EUR", 1_000_000, 925_926}, // 925925.92… rounds up, never under-counts
		{"USD", "EUR", 1, 1},
		{"USD", "JPY", 123_456, 123_456},
		{"JPY", "USD", 123, 123},
		{"JPY", "USD", 1, 1},
		{"usd", "USD", 42, 42},
		{"EUR", "USD", 0, 0},
	} {
		got, q, err := ConvertAmount(context.Background(), p, tc.from, tc.to, tc.amt)
		if err != nil || got != tc.want || q == nil {
			t.Errorf("%s->%s %d = %d, %v; want %d", tc.from, tc.to, tc.amt, got, err, tc.want)
		}
	}

	for name, call := range map[string]func() error{
		"negative":         func() error { _, _, err := ConvertAmount(context.Background(), p, "USD", "EUR", -1); return err },
		"unknown currency": func() error { _, _, err := ConvertAmount(context.Background(), p, "USD", "XXQ", 1); return err },
		"no provider":      func() error { _, _, err := ConvertAmount(context.Background(), nil, "USD", "EUR", 1); return err },
		"provider error": func() error {
			_, _, err := ConvertAmount(context.Background(), &MockProvider{Error: errors.New("down")}, "USD", "EUR", 1)
			return err
		},
		"zero rate": func() error {
			_, _, err := ConvertAmount(context.Background(), NewMockProvider(map[string]float64{"eur": 0}), "EUR", "USD", 1)
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if got, _, err := ConvertAmount(context.Background(), nil, "USD", "USD", 7); err != nil || got != 7 {
		t.Fatalf("same currency needs no provider: %d %v", got, err)
	}
}

// fxServer serves one exchange-api file per base currency, dated date, and
// counts the requests; failing bases answer 503.
type fxServer struct {
	*httptest.Server
	requests atomic.Int64
	date     string
	failing  map[string]bool
}

func newFXServer(t *testing.T, date string, failing ...string) *fxServer {
	s := &fxServer{date: date, failing: map[string]bool{}}
	for _, base := range failing {
		s.failing[base] = true
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		base := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".json")
		if s.failing[base] {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		dated := ""
		if s.date != "" {
			dated = fmt.Sprintf(`"date":%q,`, s.date)
		}
		_, _ = fmt.Fprintf(w, `{%s%q:{"usd":1.25,"eur":0.8,"gbp":0.7,"jpy":150,"xyz":3}}`, dated, base)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *fxServer) source(fallback *fxServer) *Source {
	urls := []string{s.URL + "/v1/currencies"}
	if fallback != nil {
		urls = append(urls, fallback.URL+"/v1/currencies")
	}
	return &Source{client: s.Client(), urls: urls}
}

// One file per base holds its rates to every target: one request, lower case
// on the wire, upper case in the table, unknown currencies dropped.
func TestSourceReadsOneFilePerBase(t *testing.T) {
	srv := newFXServer(t, "2026-09-01")
	var path string
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		srv.requests.Add(1)
		_, _ = w.Write([]byte(`{"date":"2026-09-01","eur":{"usd":1.08,"gbp":0.85,"jpy":160,"xyz":2}}`))
	})
	tb, err := srv.source(nil).Table(context.Background(), "EUR", []string{"USD", "GBP", "JPY", "EUR"})
	if err != nil {
		t.Fatal(err)
	}
	want := Table{Base: "EUR", AsOf: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Rates: map[string]float64{"USD": 1.08, "GBP": 0.85, "JPY": 160}}
	if path != "/v1/currencies/eur.json" || srv.requests.Load() != 1 || fmt.Sprint(tb) != fmt.Sprint(want) {
		t.Fatalf("table %+v path %q requests %d", tb, path, srv.requests.Load())
	}
}

// A failing primary is read from the fallback, one request each.
func TestSourceFallsBack(t *testing.T) {
	primary, fallback := newFXServer(t, "2026-09-01", "eur"), newFXServer(t, "2026-09-01")
	tb, err := primary.source(fallback).Table(context.Background(), "EUR", []string{"USD"})
	if err != nil || tb.Rates["USD"] != 1.25 || primary.requests.Load() != 1 || fallback.requests.Load() != 1 {
		t.Fatalf("table %+v err %v requests %d+%d", tb, err, primary.requests.Load(), fallback.requests.Load())
	}
	fallback.failing["eur"] = true
	if _, err := primary.source(fallback).Table(context.Background(), "EUR", []string{"USD"}); err == nil {
		t.Fatal("both down must fail")
	}
}

// A file without a publication date carries no AsOf, so it reads as stale.
func TestSourceUndatedTableIsStale(t *testing.T) {
	tb, err := newFXServer(t, "").source(nil).Table(context.Background(), "EUR", []string{"USD"})
	if err != nil || !tb.AsOf.IsZero() || !staleRate(tb.AsOf, time.Now()) {
		t.Fatalf("undated table: %+v %v", tb, err)
	}
}

// A canceled ctx stays detectable through the wrap chain.
func TestSourceCanceledContextIsDetectable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newFXServer(t, "2026-09-01").source(nil).Table(ctx, "EUR", []string{"USD"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// Concurrent misses for one key make one call; a failure is remembered for
// the negative window, then tried again.
func TestFlightsSingleFlightAndRememberFailure(t *testing.T) {
	f := newFlights[int]()
	f.negativeTTL = 200 * time.Millisecond
	var calls atomic.Int64
	gate := make(chan struct{})
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := f.do(context.Background(), "EUR", func(context.Context) (int, error) {
				calls.Add(1)
				<-gate
				return 1, nil
			})
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}

	down := errors.New("down")
	fail := func(context.Context) (int, error) { calls.Add(1); return 0, down }
	for range 3 {
		if _, err := f.do(context.Background(), "GBP", fail); !errors.Is(err, down) {
			t.Fatalf("want the failure, got %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 inside the negative window", calls.Load())
	}
	time.Sleep(250 * time.Millisecond)
	_, _ = f.do(context.Background(), "GBP", fail)
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want a retry after the window", calls.Load())
	}
}
