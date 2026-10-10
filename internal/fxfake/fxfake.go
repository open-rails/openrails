// Package fxfake is a loopback exchange-api for tests (Deps.FXTransport): one
// file per base currency listing its rate to every registry currency, each
// request counted by the host it was meant for. Guard keeps a test binary
// from reaching the real one.
package fxfake

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// The exchange-api's hosts: the primary and its fallback mirror.
const (
	Primary  = "latest.currency-api.pages.dev"
	Fallback = "cdn.jsdelivr.net"
)

const hostHeader = "X-Fxfake-Host"

// Server is one fake exchange-api.
type Server struct {
	server    *httptest.Server
	transport http.RoundTripper
	mu        sync.Mutex
	seen      map[string]int // host/base
	down      map[string]bool
	dates     map[string]string
}

// New starts a Server; Close stops it.
func New() *Server {
	s := &Server{seen: map[string]int{}, down: map[string]bool{}, dates: map[string]string{}}
	codes := moneyutil.CurrencyCodes()
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get(hostHeader)
		base := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".json")
		s.mu.Lock()
		s.seen[host+"/"+base]++
		down, date := s.down[host+"/"+base], s.dates[base]
		s.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if date == "" {
			date = time.Now().UTC().Format(time.DateOnly)
		}
		rates := make([]string, 0, len(codes))
		for i, code := range codes {
			if c := strings.ToLower(code); c != base {
				rates = append(rates, fmt.Sprintf("%q:%d.5", c, i+1))
			}
		}
		_, _ = fmt.Fprintf(w, `{"date":%q,%q:{%s}}`, date, base, strings.Join(rates, ","))
	}))
	target, _ := url.Parse(s.server.URL)
	wire := &http.Transport{}
	s.transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.Header.Set(hostHeader, r.URL.Host)
		out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, ""
		return wire.RoundTrip(out)
	})
	return s
}

// Close stops the server.
func (s *Server) Close() { s.server.Close() }

// Transport sends every request to this server, as Deps.FXTransport.
func (s *Server) Transport() http.RoundTripper { return s.transport }

// Requests is how many files host served; base (lower case) narrows it to one
// currency.
func (s *Server) Requests(host, base string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for key, count := range s.seen {
		h, b, _ := strings.Cut(key, "/")
		if h == host && (base == "" || b == base) {
			n += count
		}
	}
	return n
}

// Fail makes host answer 503 for base (lower case).
func (s *Server) Fail(host, base string) {
	s.mu.Lock()
	s.down[host+"/"+base] = true
	s.mu.Unlock()
}

// Date publishes base's file (lower case) as of date; the default is today.
func (s *Server) Date(base string, date time.Time) {
	s.mu.Lock()
	s.dates[base] = date.UTC().Format(time.DateOnly)
	s.mu.Unlock()
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ErrEscaped is the refusal of a request for the real exchange-api.
var ErrEscaped = errors.New("fxfake: an FX request left the test process; give the engine Deps.FXTransport")

// Guard refuses, through http.DefaultTransport, every request for the real
// exchange-api, which is where a Source without Deps.FXTransport goes. escaped
// lists them; a test binary fails when it is not empty. DefaultTransport stays
// an *http.Transport: callers clone it.
func Guard() (escaped func() []string) {
	var mu sync.Mutex
	var seen []string
	guarded := http.DefaultTransport.(*http.Transport).Clone()
	proxy := guarded.Proxy
	guarded.Proxy = func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == Primary || r.URL.Host == Fallback {
			mu.Lock()
			seen = append(seen, r.Method+" "+r.URL.String())
			mu.Unlock()
			return nil, ErrEscaped
		}
		if proxy == nil {
			return nil, nil
		}
		return proxy(r)
	}
	http.DefaultTransport = guarded
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}
