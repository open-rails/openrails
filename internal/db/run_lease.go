package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// RunLeaseTTL is how long a run's lease outlives its last renewal: a holder
// that dies frees its work within it.
const RunLeaseTTL = 2 * time.Minute

// ErrLeaseLost ends a run whose lease another run took, or whose renewal was
// not confirmed in time to be sure it had not lapsed.
var ErrLeaseLost = errors.New("db: run lease lost")

// MerchantLeaseKind names a merchant's exclusive work.
type MerchantLeaseKind string

// LeasePull is one mutating provider pull of the merchant at a time.
const LeasePull MerchantLeaseKind = "pull"

// RunLease is one run's hold on exclusive work: a row it renews every quarter
// ttl while the work runs. It holds no session and pins no connection; a
// holder that dies stops renewing and loses it at its expiry.
type RunLease struct {
	ctx  context.Context
	stop func()
}

// Context is the run's context: canceled with ErrLeaseLost once the lease is
// lost, a fifth of its ttl before it could lapse.
func (l *RunLease) Context() context.Context { return l.ctx }

// Release stops the renewals and frees the lease for the next run.
func (l *RunLease) Release() { l.stop() }

// TryPassLease takes the deployment-wide lease on a running pass of kind,
// across every fleet. It is nil while another run holds it.
func (d *DB) TryPassLease(ctx context.Context, kind string, ttl time.Duration) (*RunLease, error) {
	key, holder, q := kind+".run", uuidutil.NewV7().String(), d.GenDirectory()
	return d.holdRunLease(ctx, ttl, func(ctx context.Context) (bool, error) {
		n, err := q.ClaimPeriodicLease(ctx, gen.ClaimPeriodicLeaseParams{Kind: key, Holder: holder, TtlSeconds: ttl.Seconds()})
		return n > 0, err
	}, func(ctx context.Context) error {
		return q.ReleasePeriodicLease(ctx, gen.ReleasePeriodicLeaseParams{Kind: key, Holder: holder})
	})
}

// TryMerchantLease takes the merchant's lease on kind, across every replica
// and the operator's CLI. It is nil while another run holds it.
func (d *DB) TryMerchantLease(ctx context.Context, merchant uuid.UUID, kind MerchantLeaseKind, ttl time.Duration) (*RunLease, error) {
	holder, q := uuidutil.NewV7(), d.GenDirectory()
	return d.holdRunLease(ctx, ttl, func(ctx context.Context) (bool, error) {
		n, err := q.ClaimMerchantLease(ctx, gen.ClaimMerchantLeaseParams{MerchantID: merchant, Kind: string(kind), Holder: holder, TtlSeconds: ttl.Seconds()})
		return n > 0, err
	}, func(ctx context.Context) error {
		return q.ReleaseMerchantLease(ctx, gen.ReleaseMerchantLeaseParams{MerchantID: merchant, Kind: string(kind), Holder: holder})
	})
}

func (d *DB) holdRunLease(ctx context.Context, ttl time.Duration, claim func(context.Context) (bool, error), release func(context.Context) error) (*RunLease, error) {
	if d == nil || d.pool == nil {
		return nil, errors.New("db: a run lease needs a pool-backed DB")
	}
	return holdRunLease(ctx, logx.Or(d.Log), ttl, claim, release)
}

// holdRunLease claims and, once held, renews by claim again; release ends it.
func holdRunLease(ctx context.Context, log *slog.Logger, ttl time.Duration, claim func(context.Context) (bool, error), release func(context.Context) error) (*RunLease, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("db: run lease ttl %s", ttl)
	}
	// The lease runs from at least when its claim was sent, on the database's
	// clock.
	confirmed := time.Now()
	held, err := claim(ctx)
	if err != nil || !held {
		return nil, err
	}
	run, cancel := context.WithCancelCause(ctx)
	margin := ttl / 5
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		renew := time.NewTicker(ttl / 4)
		defer renew.Stop()
		lapse := time.NewTimer(time.Until(confirmed.Add(ttl - margin)))
		defer lapse.Stop()
		for {
			select {
			case <-quit:
				return
			case <-run.Done():
				return
			case <-lapse.C:
				cancel(ErrLeaseLost)
				return
			case <-renew.C:
				sent := time.Now()
				// A renewal never runs past the lapse it could prevent.
				rctx, rcancel := context.WithDeadline(run, confirmed.Add(ttl-margin))
				held, err := claim(rctx)
				rcancel()
				switch {
				case err == nil && !held:
					cancel(ErrLeaseLost)
					return
				case err != nil:
					if run.Err() == nil {
						log.WarnContext(run, "run lease renewal failed; retrying", logx.Err(err))
					}
					continue
				}
				confirmed = sent
				lapse.Reset(time.Until(confirmed.Add(ttl - margin)))
			}
		}
	}()
	var once sync.Once
	return &RunLease{ctx: run, stop: func() {
		once.Do(func() {
			close(quit)
			<-done
			cancel(context.Canceled)
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer rcancel()
			if err := release(rctx); err != nil {
				log.WarnContext(rctx, "run lease release failed; it lapses at its expiry", logx.Err(err))
			}
		})
	}}, nil
}
