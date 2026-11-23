package fx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const (
	// RefreshInterval is how often a quoting process reads every currency's
	// rates again.
	RefreshInterval = 2 * time.Hour
	// refreshReads bounds a refresh's concurrent source reads.
	refreshReads = 8
	// maxRateJump is the factor, either way, one publication may move a rate
	// from the one it replaces. A larger move is held back until a later
	// publication repeats it.
	maxRateJump = 1.25
)

// Rates quotes from rates held in process memory. A quote that finds its rate
// missing or stale reads its base currency's table, one request however many
// ask for the pair at once. From a process's first cross-currency quote until
// Close, every currency's table is read again each RefreshInterval. A rate
// published more than 48 hours ago is never quoted: no fresh rate, no quote.
//
// A published rate more than maxRateJump from the held one is not taken: the
// held rate is quoted until it is stale, then the pair quotes nothing until a
// publication comes back within the bound or a later one repeats the move.
// The source's two hosts serve one publisher's files, so neither can vouch for
// the other, and a process's first read of a pair has nothing to check it by.
type Rates struct {
	source     *Source
	currencies []string
	interval   time.Duration
	now        func() time.Time
	flights    *flights[rate]

	mu      sync.Mutex
	rates   map[pair]rate
	jumps   map[pair]rate // held back, awaiting a later publication
	closed  bool
	stop    context.CancelFunc
	stopped chan struct{}
	Log     *slog.Logger
}

type pair struct{ from, to string }

func (p pair) key() string { return p.from + "/" + p.to }

type rate struct {
	value float64
	asOf  time.Time
}

// NewRates quotes from source's tables, judging freshness by now.
func NewRates(logger *slog.Logger, source *Source, now func() time.Time) *Rates {
	return &Rates{source: source, currencies: moneyutil.CurrencyCodes(), interval: RefreshInterval, now: now, flights: newFlights[rate](), rates: map[pair]rate{}, jumps: map[pair]rate{}, Log: logger}
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
	p := pair{from, to}
	held, ok := r.fresh(p)
	if !ok {
		var err error
		if held, err = r.flights.do(ctx, p.key(), func(ctx context.Context) (rate, error) { return r.read(ctx, p) }); err != nil {
			return nil, fmt.Errorf("FX rate unavailable for %s -> %s: %w", from, to, err)
		}
	}
	return &Quote{FromCurrency: from, ToCurrency: to, Rate: held.value, AsOf: held.asOf}, nil
}

func (r *Rates) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	return r.Quote(ctx, currency, money.DefaultCurrency)
}

// Refresh reads every currency's table, refreshReads at a time: one request
// each, two when the primary fails. A base that fails keeps its held rates,
// as does a pair whose rate is held back; the error names how many bases
// were not wholly taken.
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
				refused := r.hold(t)
				for to := range t.Rates {
					if refused[to] == nil {
						r.flights.forget(pair{base, to}.key())
					}
				}
				for _, why := range refused {
					err = errors.Join(err, why)
				}
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
				r.Log.WarnContext(ctx, "fx: refresh failed; held rates are quoted until 48 hours after publication", logx.Err(err))
			}
		}
	}
}

// read is p's rate for a quote that missed it: held when a read just
// finished, else from its base's table.
func (r *Rates) read(ctx context.Context, p pair) (rate, error) {
	if held, ok := r.fresh(p); ok {
		return held, nil
	}
	t, err := r.fetch(ctx, p.from)
	if err != nil {
		return rate{}, err
	}
	refused := r.hold(t)
	if held, ok := r.fresh(p); ok {
		return held, nil
	}
	if why := refused[p.to]; why != nil {
		return rate{}, why
	}
	return rate{}, errors.New("the source lists none")
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

// hold takes t's rates, each unless a later publication is held or it jumps
// more than maxRateJump from the held rate. A jump is taken once a later
// publication repeats it within the bound. hold answers why each rate it
// held back was.
func (r *Rates) hold(t Table) map[string]error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var refused map[string]error
	for to, value := range t.Rates {
		p, next := pair{t.Base, to}, rate{value, t.AsOf}
		held, ok := r.rates[p]
		if ok && held.asOf.After(next.asOf) {
			continue
		}
		jump, jumped := r.jumps[p]
		switch {
		case !ok || near(value, held.value):
		case jumped && next.asOf.After(jump.asOf) && near(value, jump.value):
		default:
			if !jumped || !near(value, jump.value) {
				r.jumps[p] = next
			}
			if refused == nil {
				refused = map[string]error{}
			}
			refused[to] = fmt.Errorf("%s -> %s %v as of %s held back: more than %vx from %v as of %s", p.from, p.to, value, next.asOf.Format(time.DateOnly), maxRateJump, held.value, held.asOf.Format(time.DateOnly))
			continue
		}
		r.rates[p] = next
		delete(r.jumps, p)
	}
	return refused
}

func near(a, b float64) bool { return a <= b*maxRateJump && b <= a*maxRateJump }

// fresh is p's held rate while it is fresh.
func (r *Rates) fresh(p pair) (rate, bool) {
	r.mu.Lock()
	held, ok := r.rates[p]
	r.mu.Unlock()
	if !ok || staleRate(held.asOf, r.now()) {
		return rate{}, false
	}
	return held, true
}
