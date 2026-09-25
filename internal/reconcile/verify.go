package reconcile

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Verifier resolves unverified subscriptions from provider reads (#1089 §12).
// Reads are non-destructive, so there is no durable queue: a row stays
// unverified until a read settles it, and a missed or crashed read is
// re-detected by the next notification or pass. Duplicate reads, across
// replicas or passes, are harmless.
//
// Requests arrive from the unverified trigger's NOTIFY (Listen) and from the
// refresh pass (Pass). A merchant's requests coalesce for Coalesce, then
// bounded workers read them verifyBatchSize at a time; above BulkThreshold
// pending rows (an import) the account is read in bulk instead.
type Verifier struct {
	DB            *db.DB
	Clock         clockwork.Clock
	Builder       MerchantFetcherBuilder
	DeferDelete   subscriptions.ProviderCancelScheduler
	Notifications *subscriptions.NotificationService

	// Workers caps concurrent provider reads (NMI documents no rate limit).
	Workers int
	// Coalesce is how long a lone request waits to join others.
	Coalesce time.Duration
	// BulkThreshold switches an account to the bulk read above this many
	// pending rows.
	BulkThreshold int
	// Retries bounds retries of a transiently failing read.
	Retries int

	once     sync.Once
	mu       sync.Mutex
	pending  map[merchant.ID]map[uuid.UUID]struct{}
	inflight map[uuid.UUID]struct{}
	busy     map[merchant.ID]int
	timer    map[merchant.ID]bool
	// bulking holds, per merchant with a bulk read running, the rows it
	// covers: requests for them are dropped, the rest wait for it to end.
	bulking map[merchant.ID]map[uuid.UUID]struct{}
	sem     chan struct{}
	changed chan struct{}
	ctx     context.Context
	stop    context.CancelFunc
}

const (
	defaultVerifyWorkers  = 4
	defaultVerifyCoalesce = 300 * time.Millisecond
	defaultBulkThreshold  = 200
	defaultVerifyRetries  = 3
	verifyBatchSize       = 50
	verifyReadTimeout     = 2 * time.Minute
	bulkReadTimeout       = 30 * time.Minute
	// unverifiedChannel prefixes the per-schema NOTIFY channel of migration 0014.
	unverifiedChannel = "openrails_unverified:"
)

func (v *Verifier) init() {
	v.once.Do(func() {
		if v.Workers <= 0 {
			v.Workers = defaultVerifyWorkers
		}
		if v.Coalesce <= 0 {
			v.Coalesce = defaultVerifyCoalesce
		}
		if v.BulkThreshold <= 0 {
			v.BulkThreshold = defaultBulkThreshold
		}
		if v.Retries <= 0 {
			v.Retries = defaultVerifyRetries
		}
		v.Clock = timeutil.FirstClock(v.Clock)
		v.pending = map[merchant.ID]map[uuid.UUID]struct{}{}
		v.inflight = map[uuid.UUID]struct{}{}
		v.busy = map[merchant.ID]int{}
		v.timer = map[merchant.ID]bool{}
		v.bulking = map[merchant.ID]map[uuid.UUID]struct{}{}
		v.sem = make(chan struct{}, v.Workers)
		v.changed = make(chan struct{})
		v.ctx, v.stop = context.WithCancel(context.Background())
	})
}

// Start listens for the unverified trigger's notifications until Close.
func (v *Verifier) Start() {
	v.init()
	go v.Listen(v.ctx)
}

// Close stops in-flight reads; their rows stay unverified for the next pass.
func (v *Verifier) Close() {
	v.init()
	v.stop()
}

// Enqueue asks for an immediate read of the subscriptions. It never blocks.
func (v *Verifier) Enqueue(mid merchant.ID, ids ...uuid.UUID) {
	v.init()
	if len(ids) == 0 {
		return
	}
	v.mu.Lock()
	set := v.pending[mid]
	if set == nil {
		set = map[uuid.UUID]struct{}{}
		v.pending[mid] = set
	}
	covered, bulking := v.bulking[mid]
	for _, id := range ids {
		if _, ok := covered[id]; !ok {
			set[id] = struct{}{}
		}
	}
	if bulking {
		v.mu.Unlock()
		return
	}
	now := len(set) > v.BulkThreshold // a burst goes to bulk at once
	arm := !now && !v.timer[mid]
	if arm {
		v.timer[mid] = true
	}
	v.mu.Unlock()
	switch {
	case now:
		v.flush(mid)
	case arm:
		time.AfterFunc(v.Coalesce, func() { v.flush(mid) })
	}
}

// flush hands pending requests to workers: one bulk read above the
// threshold, otherwise batches of verifyBatchSize. A row already being read
// waits for that read to finish (per-subscription coalescing).
func (v *Verifier) flush(mid merchant.ID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.timer[mid] = false
	if _, bulking := v.bulking[mid]; bulking {
		return
	}
	set := v.pending[mid]
	if len(set) > v.BulkThreshold {
		delete(v.pending, mid)
		v.bulking[mid] = map[uuid.UUID]struct{}{}
		v.busy[mid]++
		go v.runBulk(mid)
		return
	}
	for len(set) > 0 {
		var batch []uuid.UUID
		for id := range set {
			if _, reading := v.inflight[id]; reading {
				continue
			}
			batch = append(batch, id)
			if len(batch) == verifyBatchSize {
				break
			}
		}
		if len(batch) == 0 {
			return
		}
		for _, id := range batch {
			delete(set, id)
			v.inflight[id] = struct{}{}
		}
		v.busy[mid]++
		go v.runBatch(mid, batch)
	}
}

func (v *Verifier) done(mid merchant.ID, ids []uuid.UUID) {
	v.mu.Lock()
	for _, id := range ids {
		delete(v.inflight, id)
	}
	v.busy[mid]--
	again := len(v.pending[mid]) > 0 && !v.timer[mid]
	close(v.changed)
	v.changed = make(chan struct{})
	v.mu.Unlock()
	if again {
		v.flush(mid)
	}
}

func (v *Verifier) runBatch(mid merchant.ID, ids []uuid.UUID) {
	defer v.done(mid, ids)
	select {
	case v.sem <- struct{}{}:
	case <-v.ctx.Done():
		return
	}
	defer func() { <-v.sem }()
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(v.ctx, verifyReadTimeout)
		err := recovered(func() error { return v.verifyBatch(ctx, mid, ids) })
		cancel()
		if err == nil || v.ctx.Err() != nil {
			return
		}
		if attempt+1 >= v.Retries {
			log.WithError(err).WithField("merchant_id", mid.String()).Warn("verify: provider read failed; the rows stay unverified for the next pass")
			return
		}
		select {
		case <-time.After(time.Duration(attempt+1) * time.Second):
		case <-v.ctx.Done():
			return
		}
	}
}

func (v *Verifier) runBulk(mid merchant.ID) {
	defer v.done(mid, nil)
	defer v.endBulk(mid)
	select {
	case v.sem <- struct{}{}:
	case <-v.ctx.Done():
		return
	}
	defer func() { <-v.sem }()
	ctx, cancel := context.WithTimeout(v.ctx, bulkReadTimeout)
	defer cancel()
	if err := recovered(func() error { return v.bulkRead(ctx, mid) }); err != nil {
		log.WithError(err).WithField("merchant_id", mid.String()).Warn("verify: bulk read failed; it resumes from its checkpoint on the next pass")
	}
}

// Drain waits until every request for the merchant has been read.
func (v *Verifier) Drain(ctx context.Context, mid merchant.ID) error {
	v.init()
	for {
		v.mu.Lock()
		_, bulking := v.bulking[mid]
		idle := len(v.pending[mid]) == 0 && v.busy[mid] == 0 && !v.timer[mid] && !bulking
		changed := v.changed
		v.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Listen feeds the unverified trigger's notifications to Enqueue until ctx
// ends, reconnecting on failure. It holds one pool connection.
func (v *Verifier) Listen(ctx context.Context) {
	v.init()
	pool := v.DB.Pool()
	if pool == nil {
		return
	}
	channel := pgx.Identifier{unverifiedChannel + v.DB.DataPool().Schema()}.Sanitize()
	for ctx.Err() == nil {
		if err := recovered(func() error { return v.listenOnce(ctx, channel) }); err != nil && ctx.Err() == nil {
			log.WithError(err).Warn("verify: unverified notifications interrupted; reconnecting")
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (v *Verifier) listenOnce(ctx context.Context, channel string) error {
	conn, err := v.DB.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// A LISTENing session is never handed back to the pool.
		_ = conn.Conn().Close(context.WithoutCancel(ctx))
		conn.Release()
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		m, s, ok := strings.Cut(n.Payload, ":")
		if !ok {
			continue
		}
		mid, merr := uuid.Parse(m)
		sid, serr := uuid.Parse(s)
		if merr == nil && serr == nil {
			v.Enqueue(merchant.ID(mid), sid)
		}
	}
}

func (v *Verifier) lifecycle() *subscriptions.SubscriptionLifecycleService {
	lc := subscriptions.NewSubscriptionLifecycleService(v.DB, nil, nil, nil, v.Notifications, nil, v.Clock)
	if v.DeferDelete != nil {
		lc.SetProviderCancelScheduler(v.DeferDelete)
	}
	return lc
}

// verifyBatch reads and resolves one batch of one merchant's rows.
func (v *Verifier) verifyBatch(ctx context.Context, mid merchant.ID, ids []uuid.UUID) error {
	return v.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		now := v.Clock.Now().UTC()
		subs, err := loadUnverified(ctx, v.DB, mid, ids)
		if err != nil || len(subs) == 0 {
			return err
		}
		armed := v.Builder.Build(ctx, mid)
		lc := v.lifecycle()
		var nmiSubs []*models.Subscription
		var readErr error
		for _, sub := range subs {
			if rails.IsNMI(sub.Rail) {
				nmiSubs = append(nmiSubs, sub)
				continue
			}
			prober := armed.Probers[Provider(sub.Rail)]
			if prober == nil {
				continue
			}
			snap, err := prober.ProbeSubscription(ctx, ProbeSubject{LocalID: sub.ID, RailSubscriptionID: sub.RailSubscriptionID,
				PeriodStart: sub.CurrentPeriodStartsAt, PeriodEnd: sub.CurrentPeriodEndsAt, ObservedAt: now})
			if err == nil {
				_, err = ConvergeSubscriptionFromSnapshot(ctx, v.DB, lc, sub, snap, now, 0)
			}
			readErr = errors.Join(readErr, err)
		}
		if len(nmiSubs) > 0 {
			reader, psp := nmiReader(armed)
			if reader != nil {
				readErr = errors.Join(readErr, resolveNMIBatch(ctx, v.DB, lc, reader, forPSP(nmiSubs, psp), now))
			}
		}
		recordReads(ctx, v.DB, mid, subs, now, readErr)
		return readErr
	})
}

// loadUnverified loads the rows of ids that are still unverified and read
// from a provider (engine rows are resolved by the collection engine).
func loadUnverified(ctx context.Context, database *db.DB, mid merchant.ID, ids []uuid.UUID) ([]*models.Subscription, error) {
	rows, err := database.Qx(ctx).Query(ctx, `SELECT id FROM openrails.subscriptions
		WHERE merchant_id = $1 AND id = ANY($2) AND status = 'unverified' AND deleted_at IS NULL
		  AND collection_policy <> 'engine' AND rail_subscription_id <> ''`, mid.UUID(), ids)
	if err != nil {
		return nil, fmt.Errorf("verify: list unverified: %w", err)
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("verify: list unverified: %w", err)
	}
	repo := subscriptions.NewSubscriptionRepo(database)
	out := make([]*models.Subscription, 0, len(live))
	for _, id := range live {
		sub, err := repo.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("verify: load %s: %w", id, err)
		}
		out = append(out, sub)
	}
	return out, nil
}

// recordReads stamps the provider reads on the rows' verification records.
func recordReads(ctx context.Context, database *db.DB, mid merchant.ID, subs []*models.Subscription, now time.Time, readErr error) {
	ids := make([]uuid.UUID, 0, len(subs))
	for _, s := range subs {
		ids = append(ids, s.ID)
	}
	var msg *string
	if readErr != nil {
		m := readErr.Error()
		msg = &m
	}
	if _, err := database.Qx(ctx).Exec(ctx, `UPDATE openrails.subscription_verifications
		SET reads = reads + 1, last_read_at = $3, last_error = $4
		WHERE merchant_id = $1 AND subscription_id = ANY($2)`, mid.UUID(), ids, now, msg); err != nil {
		log.WithContext(ctx).WithError(err).Warn("verify: could not record the reads")
	}
}

func forPSP(subs []*models.Subscription, psp uuid.UUID) []*models.Subscription {
	out := subs[:0:0]
	for _, s := range subs {
		if s.PspID == psp {
			out = append(out, s)
		}
	}
	return out
}

// Pass is the refresh pass's verification of the merchant's unverified NMI
// rows: a bulk read above BulkThreshold, otherwise every row is enqueued as
// the listing reaches it and the pass waits for the reads. Must run on a
// merchant-scoped connection.
func (v *Verifier) Pass(ctx context.Context, mid merchant.ID) error {
	v.init()
	var ids []uuid.UUID
	rows, err := v.DB.Qx(ctx).Query(ctx, `SELECT id FROM openrails.subscriptions
		WHERE merchant_id = $1 AND rail = 'nmi' AND status = 'unverified' AND deleted_at IS NULL
		  AND collection_policy <> 'engine' AND rail_subscription_id <> ''
		ORDER BY current_period_ends_at NULLS FIRST`, mid.UUID())
	if err != nil {
		return fmt.Errorf("verify pass: list unverified: %w", err)
	}
	if ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
		return fmt.Errorf("verify pass: list unverified: %w", err)
	}
	if len(ids) > v.BulkThreshold {
		return v.Bulk(ctx, mid)
	}
	for len(ids) > 0 {
		n := min(len(ids), verifyBatchSize)
		v.Enqueue(mid, ids[:n]...)
		ids = ids[n:]
	}
	return v.Drain(ctx, mid)
}

// cover drops pending requests for the rows a running bulk read covers.
func (v *Verifier) cover(mid merchant.ID, subs []*models.Subscription) {
	v.mu.Lock()
	defer v.mu.Unlock()
	covered := v.bulking[mid]
	if covered == nil {
		return
	}
	for _, s := range subs {
		covered[s.ID] = struct{}{}
		delete(v.pending[mid], s.ID)
	}
}

func (v *Verifier) endBulk(mid merchant.ID) {
	v.mu.Lock()
	delete(v.bulking, mid)
	again := len(v.pending[mid]) > 0 && !v.timer[mid]
	v.mu.Unlock()
	if again {
		v.flush(mid)
	}
}

// recovered turns a panic in a provider read into an error, so one bad
// response never takes the process down.
func recovered(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("verify: panic: %v\n%s", p, debug.Stack())
		}
	}()
	return fn()
}
