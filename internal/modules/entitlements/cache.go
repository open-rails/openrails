package entitlements

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// HeavyBuyerProducts is how many products a customer holds before their
// keys are cached: below it, deriving them live is a few milliseconds.
const HeavyBuyerProducts = 500

// rebuildTimeout bounds one background rebuild.
const rebuildTimeout = 2 * time.Minute

// rebuilding holds the customers whose cache is being rebuilt in this
// process, so a burst of misses starts one rebuild.
var rebuilding sync.Map

// cachedReads keeps cached reads and rebuilds on the cache index. Its statistics can lag
// a fresh rebuild; a sequential scan of the whole cache would cost what the
// cache saves.
func cachedReads(ctx context.Context, q *gen.Queries) error {
	_, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: "enable_seqscan", Value: "off", IsLocal: true})
	return err
}

// customPlans plans each derived read for its own customer. A generic plan
// prices a customer holding three products and one holding fifty thousand
// alike, and its key-range scan per held product is O(held x catalog) for
// the latter.
func customPlans(ctx context.Context, q *gen.Queries) error {
	_, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: "plan_cache_mode", Value: "force_custom_plan", IsLocal: true})
	return err
}

// cacheValid reports, in the caller's snapshot, whether the customer's cached
// keys answer at at: built at the current entitlement generation and access
// version, for a window containing at.
func cacheValid(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, at time.Time) (bool, error) {
	return q.EntitlementCacheValid(ctx, gen.EntitlementCacheValidParams{MerchantID: merchantID, CustomerID: customer, AtTime: at})
}

// heavy reports whether the customer holds enough products at at to cache.
func heavy(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, at time.Time) (bool, error) {
	held, err := q.CountHeldProductsUpTo(ctx, gen.CountHeldProductsUpToParams{MerchantID: merchantID, CustomerID: customer, AtTime: at, UpTo: HeavyBuyerProducts})
	return held >= HeavyBuyerProducts, err
}

// RebuildEntitlementCache caches the keys a heavy buyer holds now, stamped
// from the same snapshot it derives them in, and drops the cache of a
// customer who is no longer heavy. A concurrent rebuild of the same customer
// wins; this one does nothing. A write to the customer's access while it runs
// fails it, and it starts over from a fresh snapshot, rebuildAttempts times.
func (s *EntitlementService) RebuildEntitlementCache(ctx context.Context, customer uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		err = s.rebuildOnce(ctx, mid.UUID(), customer)
		if !conflicted(err) {
			return err
		}
		if attempt == rebuildAttempts {
			return nil // writers kept moving the stamps; the next read rebuilds
		}
	}
}

// rebuildAttempts bounds the fresh snapshots one rebuild takes.
const rebuildAttempts = 3

// conflicted reports a rebuild that lost to a concurrent write.
func conflicted(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01" || pgErr.Code == "23505")
}

func (s *EntitlementService) rebuildOnce(ctx context.Context, mid, customer uuid.UUID) error {
	at := s.now().UTC()
	return s.db.WriteFromSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		// First, so the snapshot is taken as the lock is.
		locked, err := q.TryLockEntitlementCache(ctx, customer)
		if err != nil || !locked {
			return err
		}
		if err := customPlans(ctx, q); err != nil {
			return err
		}
		if err := cachedReads(ctx, q); err != nil {
			return err
		}
		stamps, err := q.GetAccessStamps(ctx, gen.GetAccessStampsParams{MerchantID: mid, CustomerID: customer})
		if err != nil {
			return err
		}
		held, err := q.CountHeldProductsUpTo(ctx, gen.CountHeldProductsUpToParams{MerchantID: mid, CustomerID: customer, AtTime: at, UpTo: HeavyBuyerProducts})
		if err != nil {
			return err
		}
		if held < HeavyBuyerProducts {
			if err := q.DeleteEntitlementCache(ctx, gen.DeleteEntitlementCacheParams{MerchantID: mid, CustomerID: customer}); err != nil {
				return err
			}
			return q.DropEntitlementCacheStamps(ctx, gen.DropEntitlementCacheStampsParams{MerchantID: mid, CustomerID: customer})
		}
		synced, err := q.SyncEntitlementCache(ctx, gen.SyncEntitlementCacheParams{MerchantID: mid, CustomerID: customer, AtTime: at})
		if err != nil {
			return err
		}
		var until *time.Time
		next, err := q.GetNextAccessBoundary(ctx, gen.GetNextAccessBoundaryParams{MerchantID: mid, CustomerID: customer, AtTime: at})
		switch {
		case err == nil:
			until = next
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		return q.UpsertEntitlementCacheStamps(ctx, gen.UpsertEntitlementCacheStampsParams{
			MerchantID: mid, CustomerID: customer, EntitlementGeneration: stamps.EntitlementGeneration, AccessVersion: stamps.AccessVersion,
			ValidFrom: at, ValidUntil: until, Keys: synced.Keys, HeldProducts: held,
		})
	})
}

// rebuildAfterMiss rebuilds a heavy buyer's cache in the background after a
// read derived their keys live: the read that missed does not wait, and the
// cache it builds answers only if nothing changed meanwhile.
func (s *EntitlementService) rebuildAfterMiss(merchantID billing.MerchantID, customer uuid.UUID) {
	key := merchantID.String() + "/" + customer.String()
	if _, running := rebuilding.LoadOrStore(key, true); running {
		return
	}
	go func() {
		defer rebuilding.Delete(key)
		ctx, cancel := context.WithTimeout(merchant.WithID(context.Background(), merchantID), rebuildTimeout)
		defer cancel()
		if err := s.RebuildEntitlementCache(ctx, customer); err != nil {
			log.WithContext(ctx).WithError(err).WithField("customer_id", customer).Warn("entitlement cache rebuild failed; keys stay derived live")
		}
	}()
}

// readSnapshot is the customer's key reads in one snapshot: from the cache
// when it is valid at at, else live. rebuild: the reads ran live for a heavy
// buyer at the current instant, so the cache should be rebuilt.
type readSnapshot struct {
	q        *gen.Queries
	merchant uuid.UUID
	customer uuid.UUID
	at       time.Time
	cached   bool
}

func (s *EntitlementService) withKeys(ctx context.Context, customerID string, at time.Time, current bool, fn func(ctx context.Context, r readSnapshot) error) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	customer, err := db.ResolveCustomerID(customerID)
	if err != nil {
		return err
	}
	rebuild := false
	err = s.db.ReadSnapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(ctx)
		cached, err := cacheValid(ctx, q, mid.UUID(), customer, at)
		if err != nil {
			return err
		}
		plan := customPlans
		if cached {
			plan = cachedReads
		}
		if err := plan(ctx, q); err != nil {
			return err
		}
		if err := fn(ctx, readSnapshot{q: q, merchant: mid.UUID(), customer: customer, at: at, cached: cached}); err != nil {
			return err
		}
		if !cached && current {
			rebuild, err = heavy(ctx, q, mid.UUID(), customer, at)
		}
		return err
	})
	if err == nil && rebuild {
		s.rebuildAfterMiss(mid, customer)
	}
	return err
}
