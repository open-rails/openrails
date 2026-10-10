package fx

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const (
	// RefreshInterval is how often a quoting process reads every currency's
	// rates again.
	RefreshInterval = 2 * time.Hour
	// refreshReads bounds a refresh's concurrent source reads.
	refreshReads = 8
)

// Rates quotes from rate tables held in process memory. A quote that finds
// its base currency's table missing or stale reads it, one request however
// many ask at once. From a process's first cross-currency quote until Close,
// every currency's table is read again each RefreshInterval. A table
// published more than 48 hours ago is never quoted: no fresh rate, no quote.
type Rates struct {
	source     *Source
	currencies []string
	interval   time.Duration
	now        func() time.Time
	flights    *flights[Table]

	mu      sync.Mutex
	tables  map[string]Table
	closed  bool
	stop    context.CancelFunc
	stopped chan struct{}
}

// NewRates quotes from source's tables.
func NewRates(source *Source) *Rates {
	return &Rates{source: source, currencies: moneyutil.CurrencyCodes(), interval: RefreshInterval, now: time.Now, flights: newFlights[Table](), tables: map[string]Table{}}
}

func (r *Rates) Quote(ctx context.Context, from, to string) (*Quote, error) {
	from, to = normalizeCurrency(from), normalizeCurrency(to)
	if from == "" || to == "" {
		return nil, fmt.Errorf("from_currency and to_currency are required")
	}
	if from == to {
		return &Quote{FromCurrency: from, ToCurrency: to, Rate: 1, AsOf: r.now()}, nil
	}
	r.startRefresh()
	t, ok := r.held(from)
	if !ok {
		var err error
		if t, err = r.flights.do(ctx, from, func(ctx context.Context) (Table, error) { return r.read(ctx, from) }); err != nil {
			return nil, fmt.Errorf("FX rate unavailable for %s -> %s: %w", from, to, err)
		}
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

// Refresh reads every currency's table, refreshReads at a time: one request
// each, two when the primary fails. A base that fails keeps its held table;
// the error names how many failed.
func (r *Rates) Refresh(ctx context.Context) error {
	failures := make([]error, len(r.currencies))
	slots := make(chan struct{}, refreshReads)
	var wg sync.WaitGroup
	for i, base := range r.currencies {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			t, err := r.fetch(ctx, base)
			if err == nil {
				r.hold(t)
				r.flights.forget(base)
			}
			failures[i] = err
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var errs []error
	for _, err := range failures {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d FX base currencies failed; first %w", len(errs), len(r.currencies), errs[0])
	}
	return nil
}

// Close stops the refresh. Quotes still read what they miss.
func (r *Rates) Close() {
	r.mu.Lock()
	r.closed = true
	stop, stopped := r.stop, r.stopped
	r.mu.Unlock()
	if stop != nil {
		stop()
		<-stopped
	}
}

// startRefresh starts the periodic refresh once.
func (r *Rates) startRefresh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.stop != nil {
		return
	}
	ctx, stop := context.WithCancel(context.Background())
	r.stop, r.stopped = stop, make(chan struct{})
	go r.refreshEvery(ctx, r.stopped)
}

func (r *Rates) refreshEvery(ctx context.Context, stopped chan<- struct{}) {
	defer close(stopped)
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := r.Refresh(ctx); err != nil && ctx.Err() == nil {
				log.WithError(err).Warn("fx: refresh failed; held rates are quoted until 48 hours after publication")
			}
		}
	}
}

// read is base's table for a quote that missed it: held when a read just
// finished, else from the source.
func (r *Rates) read(ctx context.Context, base string) (Table, error) {
	if t, ok := r.held(base); ok {
		return t, nil
	}
	t, err := r.fetch(ctx, base)
	if err != nil {
		return Table{}, err
	}
	r.hold(t)
	return t, nil
}

// fetch is base's table from the source, refused when stale.
func (r *Rates) fetch(ctx context.Context, base string) (Table, error) {
	t, err := r.source.Table(ctx, base, r.currencies)
	if err != nil {
		return Table{}, err
	}
	if staleRate(t.AsOf, r.now()) {
		return Table{}, fmt.Errorf("FX rates for %s are stale (as of %s)", base, t.AsOf.Format(time.DateOnly))
	}
	return t, nil
}

// hold keeps t unless a later publication is held.
func (r *Rates) hold(t Table) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if held, ok := r.tables[t.Base]; ok && held.AsOf.After(t.AsOf) {
		return
	}
	r.tables[t.Base] = t
}

// held is base's table while it is fresh.
func (r *Rates) held(base string) (Table, bool) {
	r.mu.Lock()
	t, ok := r.tables[base]
	r.mu.Unlock()
	if !ok || staleRate(t.AsOf, r.now()) {
		return Table{}, false
	}
	return t, true
}
