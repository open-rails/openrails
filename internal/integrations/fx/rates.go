package fx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const (
	// RefreshInterval is the fleet's refresh cadence (one River job).
	RefreshInterval = 2 * time.Hour
	// rateTTL is how long a stored rate is quoted before a quote reads its
	// base currency again: a refresh missed, or none running.
	rateTTL = 3 * time.Hour
	// memoryTTL bounds how long a replica quotes a rate without reading the
	// table.
	memoryTTL = 5 * time.Minute
)

// Rates quotes from billing.fx_rates, which one refresh fills for the whole
// fleet. A pair missing or older than rateTTL is read inline, its whole base
// currency in one request, and stored for every replica. It fails closed: no
// fresh rate, no quote.
type Rates struct {
	db         *db.DB
	source     *Source
	currencies []string
	now        func() time.Time
	flights    *flights[Table]

	mu     sync.Mutex
	memory map[string]memoryRate
}

type memoryRate struct {
	quote Quote
	until time.Time
}

// NewRates quotes from d's fx_rates, reading source when it must.
func NewRates(d *db.DB, source *Source) *Rates {
	return &Rates{db: d, source: source, currencies: moneyutil.CurrencyCodes(), now: time.Now, flights: newFlights[Table](), memory: map[string]memoryRate{}}
}

func (r *Rates) Quote(ctx context.Context, from, to string) (*Quote, error) {
	from, to = normalizeCurrency(from), normalizeCurrency(to)
	if from == "" || to == "" {
		return nil, fmt.Errorf("from_currency and to_currency are required")
	}
	now := r.now()
	if from == to {
		return &Quote{FromCurrency: from, ToCurrency: to, Rate: 1, AsOf: now}, nil
	}
	if q, ok := r.recall(from, to, now); ok {
		return q, nil
	}
	row, err := r.db.GenDirectory().GetFXRate(ctx, gen.GetFXRateParams{FromCurrency: from, ToCurrency: to})
	switch {
	case err == nil && now.Sub(row.FetchedAt) < rateTTL && !staleRate(row.AsOf, now):
		return r.remember(Quote{FromCurrency: from, ToCurrency: to, Rate: row.Rate, AsOf: row.AsOf}, row.FetchedAt.Add(rateTTL), now), nil
	case err != nil && !db.IsNotFound(err):
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.WithError(err).Warn("fx: stored rates unreadable; reading the source")
	}
	// A read of the same base may have finished since the first look.
	if q, ok := r.recall(from, to, now); ok {
		return q, nil
	}
	t, err := r.flights.do(ctx, from, func(ctx context.Context) (Table, error) {
		return r.read(ctx, from)
	})
	if err != nil {
		return nil, fmt.Errorf("FX rate unavailable for %s -> %s: %w", from, to, err)
	}
	rate, ok := t.Rates[to]
	if !ok {
		return nil, fmt.Errorf("FX rate unavailable for %s -> %s: the source lists none", from, to)
	}
	return &Quote{FromCurrency: from, ToCurrency: to, Rate: rate, AsOf: t.AsOf}, nil
}

func (r *Rates) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	return r.Quote(ctx, currency, money.DefaultCurrency)
}

// refreshReads bounds a refresh's concurrent source reads.
const refreshReads = 8

// Refresh reads every currency's table, one request each, and stores every
// pair for the fleet. A base that fails keeps its stored rates; the error
// names how many failed.
func (r *Rates) Refresh(ctx context.Context) error {
	results := make([]Table, len(r.currencies))
	failures := make([]error, len(r.currencies))
	slots := make(chan struct{}, refreshReads)
	var wg sync.WaitGroup
	for i, base := range r.currencies {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			t, err := r.source.Table(ctx, base, r.currencies)
			if err == nil && staleRate(t.AsOf, r.now()) {
				err = fmt.Errorf("FX rates for %s are stale (as of %s)", base, t.AsOf.Format(time.DateOnly))
			}
			results[i], failures[i] = t, err
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var tables []Table
	var errs []error
	for i := range r.currencies {
		if failures[i] != nil {
			errs = append(errs, failures[i])
			continue
		}
		tables = append(tables, results[i])
	}
	if err := r.store(ctx, r.now(), tables...); err != nil {
		return err
	}
	for _, t := range tables {
		r.flights.forget(t.Base)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d FX base currencies failed; first %w", len(errs), len(r.currencies), errs[0])
	}
	return nil
}

// read is base's table from the source, stored for the fleet.
func (r *Rates) read(ctx context.Context, base string) (Table, error) {
	t, err := r.source.Table(ctx, base, r.currencies)
	if err != nil {
		return Table{}, err
	}
	if staleRate(t.AsOf, r.now()) {
		return Table{}, fmt.Errorf("FX rates for %s are stale (as of %s)", base, t.AsOf.Format(time.DateOnly))
	}
	now := r.now()
	if err := r.store(ctx, now, t); err != nil {
		// The quote stands; the next one reads the source again.
		log.WithError(err).Warn("fx: storing rates failed")
	}
	for to, rate := range t.Rates {
		r.remember(Quote{FromCurrency: base, ToCurrency: to, Rate: rate, AsOf: t.AsOf}, now.Add(rateTTL), now)
	}
	return t, nil
}

func (r *Rates) store(ctx context.Context, fetchedAt time.Time, tables ...Table) error {
	var p gen.PutFXRatesParams
	for _, t := range tables {
		for to, rate := range t.Rates {
			p.FromCurrencies = append(p.FromCurrencies, t.Base)
			p.ToCurrencies = append(p.ToCurrencies, to)
			p.Rates = append(p.Rates, rate)
			p.AsOfs = append(p.AsOfs, t.AsOf)
		}
	}
	if len(p.Rates) == 0 {
		return nil
	}
	p.FetchedAt = fetchedAt
	if err := r.db.GenDirectory().PutFXRates(ctx, p); err != nil {
		return errors.Join(errors.New("fx: store rates"), err)
	}
	return nil
}

// recall is a remembered rate still good at now.
func (r *Rates) recall(from, to string, now time.Time) (*Quote, bool) {
	r.mu.Lock()
	m, ok := r.memory[from+":"+to]
	r.mu.Unlock()
	if !ok || !now.Before(m.until) || staleRate(m.quote.AsOf, now) {
		return nil, false
	}
	q := m.quote
	return &q, true
}

// remember keeps q until the stored rate's own expiry, memoryTTL at most.
func (r *Rates) remember(q Quote, until, now time.Time) *Quote {
	if limit := now.Add(memoryTTL); until.After(limit) {
		until = limit
	}
	r.mu.Lock()
	r.memory[q.FromCurrency+":"+q.ToCurrency] = memoryRate{quote: q, until: until}
	r.mu.Unlock()
	return &q
}
