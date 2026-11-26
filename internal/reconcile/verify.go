package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Verifier resolves unverified subscriptions from provider reads. The
// unverified trigger records the work in subscription_verifications and sends
// a notification. The billing pass requeues due records after a missed
// notification or an inconclusive read. Both paths read only queued ids.
// An operator refresh can also ask for a bulk verification of an NMI PSP.
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
	// BulkThreshold switches a manual refresh to the bulk read above this
	// many unverified rows on an account.
	BulkThreshold int
	// Retries bounds retries of a transiently failing read.
	Retries int

	once sync.Once
	// running are Listen, coalescing timers and batch reads; Close waits
	// for them.
	running  sync.WaitGroup
	mu       sync.Mutex
	pending  map[billing.MerchantID]map[uuid.UUID]struct{}
	inflight map[uuid.UUID]struct{}
	timer    map[billing.MerchantID]bool
	// bulking holds, per merchant with a bulk read running, the rows it
	// covers: requests for them are dropped, the rest wait for it to end and
	// are then read in batches.
	bulking map[billing.MerchantID]map[uuid.UUID]struct{}
	sem     chan struct{}
	ctx     context.Context
	stop    context.CancelFunc
	Log     *slog.Logger
}

const (
	defaultVerifyWorkers   = 4
	defaultVerifyCoalesce  = 300 * time.Millisecond
	defaultBulkThreshold   = 200
	defaultVerifyRetries   = 3
	verifyBatchSize        = 50
	verifyReadTimeout      = 2 * time.Minute
	verificationScanLimit  = 500
	verificationRetryAfter = time.Hour
	// unverifiedChannel prefixes the unverified trigger's per-schema NOTIFY channel.
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
		v.pending = map[billing.MerchantID]map[uuid.UUID]struct{}{}
		v.inflight = map[uuid.UUID]struct{}{}
		v.timer = map[billing.MerchantID]bool{}
		v.bulking = map[billing.MerchantID]map[uuid.UUID]struct{}{}
		v.sem = make(chan struct{}, v.Workers)
		v.ctx, v.stop = context.WithCancel(context.Background())
	})
}

// Start listens for the unverified trigger's notifications until Close.
func (v *Verifier) Start() {
	v.init()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.spawnLocked(func() { v.Listen(v.ctx) })
}

// Close stops in-flight reads, their rows staying unverified for the next
// pass, and waits for the verifier's goroutines.
func (v *Verifier) Close() {
	v.init()
	v.mu.Lock()
	v.stop()
	v.mu.Unlock()
	v.running.Wait()
}

// spawnLocked runs fn on a goroutine Close waits for, unless Close has
// begun. v.mu is held.
func (v *Verifier) spawnLocked(fn func()) {
	if v.ctx.Err() != nil {
		return
	}
	v.running.Add(1)
	go func() {
		defer v.running.Done()
		fn()
	}()
}

// Enqueue asks for an immediate read of the subscriptions. It never blocks.
func (v *Verifier) Enqueue(mid billing.MerchantID, ids ...uuid.UUID) {
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
	if !v.timer[mid] && v.ctx.Err() == nil {
		v.timer[mid] = true
		v.running.Add(1)
		time.AfterFunc(v.Coalesce, func() {
			defer v.running.Done()
			v.flush(mid)
		})
	}
	v.mu.Unlock()
}

// flush batches pending ids for the workers. A row already being read waits
// for that read to finish.
func (v *Verifier) flush(mid billing.MerchantID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.timer[mid] = false
	if _, bulking := v.bulking[mid]; bulking || v.ctx.Err() != nil {
		return
	}
	set := v.pending[mid]
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
		v.spawnLocked(func() { v.runBatch(mid, batch) })
	}
}

func (v *Verifier) done(mid billing.MerchantID, ids []uuid.UUID) {
	v.mu.Lock()
	for _, id := range ids {
		delete(v.inflight, id)
	}
	again := len(v.pending[mid]) > 0 && !v.timer[mid]
	v.mu.Unlock()
	if again {
		v.flush(mid)
	}
}

func (v *Verifier) runBatch(mid billing.MerchantID, ids []uuid.UUID) {
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
			v.Log.WarnContext(ctx, "verify: provider read failed; the rows stay unverified for the next pass", logx.Err(err), "merchant_id", mid.String())
			return
		}
		select {
		case <-time.After(time.Duration(attempt+1) * time.Second):
		case <-v.ctx.Done():
			return
		}
	}
}

// RetryDue queues a bounded batch of durable verification records. The first
// read normally comes from LISTEN; the billing pass covers stopped listeners,
// failed reads and decisions that are still held or inconclusive.
func (v *Verifier) RetryDue(ctx context.Context) error {
	v.init()
	rows, err := v.DB.GenDirectory().ListDueSubscriptionVerifications(ctx, gen.ListDueSubscriptionVerificationsParams{
		ReadBefore: v.Clock.Now().UTC().Add(-verificationRetryAfter), RowLimit: verificationScanLimit,
	})
	if err != nil {
		return fmt.Errorf("verify: list due subscription verifications: %w", err)
	}
	byMerchant := map[billing.MerchantID][]uuid.UUID{}
	for _, row := range rows {
		mid := billing.MerchantID(row.MerchantID)
		byMerchant[mid] = append(byMerchant[mid], row.SubscriptionID)
	}
	for mid, ids := range byMerchant {
		v.Enqueue(mid, ids...)
	}
	return nil
}

// Listen feeds the unverified trigger's notifications to Enqueue until ctx
// ends, on a session of its own. Each (re)connect queues the rows no read has
// reached, which a notification missed while nothing listened.
func (v *Verifier) Listen(ctx context.Context) {
	v.init()
	if v.DB.Pool() == nil {
		return
	}
	v.DB.Listen(ctx, db.Listener{
		Channel: unverifiedChannel + v.DB.DataPool().Schema(),
		Connected: func(ctx context.Context) {
			if err := recovered(func() error { return v.queueUnread(ctx) }); err != nil && ctx.Err() == nil {
				v.Log.WarnContext(ctx, "verify: unread verifications not queued; the billing pass retries them", logx.Err(err))
			}
		},
		Notified: func(ctx context.Context, payload string) {
			m, s, ok := strings.Cut(payload, ":")
			if !ok {
				return
			}
			mid, merr := uuid.Parse(m)
			sid, serr := uuid.Parse(s)
			if merr == nil && serr == nil {
				v.Enqueue(billing.MerchantID(mid), sid)
			}
		},
		Interrupted: func(ctx context.Context, err error) {
			v.Log.WarnContext(ctx, "verify: unverified notifications interrupted; reconnecting", logx.Err(err))
		},
	})
}

// queueUnread queues a bounded batch of the rows no read has reached.
func (v *Verifier) queueUnread(ctx context.Context) error {
	rows, err := v.DB.GenDirectory().ListUnreadSubscriptionVerifications(ctx, verificationScanLimit)
	if err != nil {
		return fmt.Errorf("verify: list unread subscription verifications: %w", err)
	}
	byMerchant := map[billing.MerchantID][]uuid.UUID{}
	for _, row := range rows {
		mid := billing.MerchantID(row.MerchantID)
		byMerchant[mid] = append(byMerchant[mid], row.SubscriptionID)
	}
	for mid, ids := range byMerchant {
		v.Enqueue(mid, ids...)
	}
	return nil
}

func (v *Verifier) lifecycle() *subscriptions.SubscriptionLifecycleService {
	lc := subscriptions.NewSubscriptionLifecycleService(v.DB, nil, nil, nil, v.Notifications, nil, v.Clock)
	lc.SetConfig(v.Builder.Config)
	if v.DeferDelete != nil {
		lc.SetProviderCancelScheduler(v.DeferDelete)
	}
	return lc
}

// verifyBatch reads and resolves one batch of one merchant's rows, each with
// the PSP that owns it.
func (v *Verifier) verifyBatch(ctx context.Context, mid billing.MerchantID, ids []uuid.UUID) error {
	return v.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		now := v.Clock.Now().UTC()
		subs, err := loadUnverified(ctx, v.DB, mid, ids)
		if err != nil || len(subs) == 0 {
			return err
		}
		lc := v.lifecycle()
		var readErr error
		for _, group := range byPSP(subs) {
			readErr = errors.Join(readErr, v.verifyPSP(ctx, mid, lc, group, now))
		}
		recordReads(ctx, v.DB, mid, subs, now, readErr)
		return readErr
	})
}

// verifyPSP reads one PSP's rows with that PSP's own credentials, archived or
// not. A PSP that cannot arm reads nothing, so nothing is decided.
func (v *Verifier) verifyPSP(ctx context.Context, mid billing.MerchantID, lc *subscriptions.SubscriptionLifecycleService, subs []*models.Subscription, now time.Time) error {
	psp := subs[0].PspID
	armed, provider, ok := v.Builder.BuildPSP(ctx, mid, psp)
	if !ok {
		return fmt.Errorf("verify: PSP %s is not armed; its rows stay unverified", psp)
	}
	if provider == ProviderNMI {
		reader, _ := NMIReader(armed)
		return resolveNMIBatch(ctx, v.DB, lc, reader, psp, subs, now)
	}
	prober := armed.Probers[provider]
	if prober == nil {
		return fmt.Errorf("verify: PSP %s has no %s prober; its rows stay unverified", psp, provider)
	}
	var readErr error
	snaps := make(map[uuid.UUID]*RemoteSnapshot, len(subs))
	for _, sub := range subs {
		snap, err := prober.ProbeSubscription(ctx, ProbeSubject{LocalID: sub.ID, RailSubscriptionID: sub.RailSubscriptionID,
			PeriodStart: sub.CurrentPeriodStartsAt, PeriodEnd: sub.CurrentPeriodEndsAt, ObservedAt: now})
		if err != nil {
			readErr = errors.Join(readErr, err)
			continue
		}
		snaps[sub.ID] = snap
	}
	return errors.Join(readErr, applyVerified(ctx, v.DB, lc, provider, psp, subs, snaps, now))
}

// byPSP groups rows by the PSP that owns them, in first-seen order.
func byPSP(subs []*models.Subscription) [][]*models.Subscription {
	index := map[uuid.UUID]int{}
	var out [][]*models.Subscription
	for _, s := range subs {
		i, ok := index[s.PspID]
		if !ok {
			i = len(out)
			index[s.PspID] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], s)
	}
	return out
}

// loadUnverified loads the rows of ids that are still unverified and read
// from a provider (engine rows are resolved by the collection engine).
func loadUnverified(ctx context.Context, database *db.DB, mid billing.MerchantID, ids []uuid.UUID) ([]*models.Subscription, error) {
	live, err := database.Gen(ctx).ListUnverifiedSubscriptionIDsIn(ctx, gen.ListUnverifiedSubscriptionIDsInParams{MerchantID: mid.UUID(), Ids: ids})
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
func recordReads(ctx context.Context, database *db.DB, mid billing.MerchantID, subs []*models.Subscription, now time.Time, readErr error) {
	ids := make([]uuid.UUID, 0, len(subs))
	for _, s := range subs {
		ids = append(ids, s.ID)
	}
	var msg *string
	if readErr != nil {
		m := readErr.Error()
		msg = &m
	}
	if err := database.Gen(ctx).RecordSubscriptionVerificationReads(ctx, gen.RecordSubscriptionVerificationReadsParams{
		MerchantID: mid.UUID(), SubscriptionIds: ids, ReadAt: now, LastError: msg,
	}); err != nil {
		database.Log.WarnContext(ctx, "verify: could not record the reads", logx.Err(err))
	}
}

// Pass verifies the merchant's unverified NMI rows as part of a manual
// refresh. Large PSPs use the bulk read. The other PSPs use scoped batches
// and return read failures to the operator's job. Must run merchant-scoped.
func (v *Verifier) Pass(ctx context.Context, mid billing.MerchantID) error {
	v.init()
	counts, err := v.DB.Gen(ctx).CountUnverifiedNMISubscriptionsByPSP(ctx, mid.UUID())
	if err != nil {
		return fmt.Errorf("verify pass: count unverified: %w", err)
	}
	var errs error
	bulk := false
	for _, c := range counts {
		if c.Rows > int64(v.BulkThreshold) {
			bulk = true
			continue
		}
		ids, err := v.DB.Gen(ctx).ListUnverifiedSubscriptionIDsForPSP(ctx, gen.ListUnverifiedSubscriptionIDsForPSPParams{MerchantID: mid.UUID(), PspID: c.PspID})
		if err != nil {
			return fmt.Errorf("verify pass: list unverified: %w", err)
		}
		for len(ids) > 0 {
			n := min(len(ids), verifyBatchSize)
			errs = errors.Join(errs, v.verifyBatch(ctx, mid, ids[:n]))
			ids = ids[n:]
		}
	}
	if bulk {
		errs = errors.Join(errs, v.Bulk(ctx, mid))
	}
	return errs
}

// cover drops pending requests for the rows a running bulk read covers.
func (v *Verifier) cover(mid billing.MerchantID, subs []*models.Subscription) {
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

func (v *Verifier) endBulk(mid billing.MerchantID) {
	v.mu.Lock()
	delete(v.bulking, mid)
	again := len(v.pending[mid]) > 0 && !v.timer[mid]
	v.mu.Unlock()
	if again {
		v.flush(mid) // what the bulk read did not cover
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
