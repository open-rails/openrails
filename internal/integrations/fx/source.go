package fx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/httpx"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// The CC0 exchange-api (github.com/fawazahmed0/exchange-api): no key, no
// attribution. One file per base currency holds its rate to every other.
const (
	primaryURL  = "https://latest.currency-api.pages.dev/v1/currencies"
	fallbackURL = "https://cdn.jsdelivr.net/npm/@fawazahmed0/currency-api@latest/v1/currencies"
)

// Table is one base currency's published rates.
type Table struct {
	Base string
	// AsOf is the publication date; zero when the file has none, which reads
	// as stale.
	AsOf  time.Time
	Rates map[string]float64
}

// Source reads rate tables from the exchange-api, the fallback mirror when the
// primary fails.
type Source struct {
	client *http.Client
	urls   []string
}

// NewSource reads the exchange-api over transport; nil is
// http.DefaultTransport.
func NewSource(transport http.RoundTripper) *Source {
	return &Source{client: &http.Client{Timeout: 5 * time.Second, Transport: transport}, urls: []string{primaryURL, fallbackURL}}
}

// Table is base's rates to each of targets the file lists: one request, two
// when the primary fails.
func (s *Source) Table(ctx context.Context, base string, targets []string) (Table, error) {
	base = normalizeCurrency(base)
	var err error
	for _, url := range s.urls {
		var t Table
		if t, err = s.read(ctx, url, base, targets); err == nil {
			return t, nil
		}
	}
	return Table{}, fmt.Errorf("FX rates for %s: %w", base, err)
}

func (s *Source) read(ctx context.Context, baseURL, base string, targets []string) (Table, error) {
	// The wire is lower case, in the path and the keys.
	wire := strings.ToLower(base)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/"+wire+".json", nil)
	if err != nil {
		return Table{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Table{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Table{}, fmt.Errorf("unexpected status: %s", resp.Status)
	}
	var raw map[string]json.RawMessage
	// A money-affecting path never reads an unbounded body.
	if err := httpx.DecodeJSONLimited(resp.Body, 0, &raw); err != nil {
		return Table{}, fmt.Errorf("decode: %w", err)
	}
	t := Table{Base: base, Rates: map[string]float64{}}
	var date string
	if dateRaw, ok := raw["date"]; ok {
		if err := json.Unmarshal(dateRaw, &date); err != nil {
			return Table{}, fmt.Errorf("decode date: %w", err)
		}
	}
	if date != "" {
		if parsed, err := timeutil.ParseDateUTC(date); err == nil {
			t.AsOf = parsed
		}
	}
	ratesRaw, ok := raw[wire]
	if !ok {
		return Table{}, fmt.Errorf("currency %s not found in response", wire)
	}
	var rates map[string]float64
	if err := json.Unmarshal(ratesRaw, &rates); err != nil {
		return Table{}, fmt.Errorf("decode rates: %w", err)
	}
	for _, target := range targets {
		target = normalizeCurrency(target)
		if rate, ok := rates[strings.ToLower(target)]; ok && target != base && rate > 0 {
			t.Rates[target] = rate
		}
	}
	return t, nil
}
