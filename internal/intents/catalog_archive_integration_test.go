//go:build integration

package intents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// archiveFixture seeds nothing catalog-side by default: the remote object is
// an extra (not in the local catalog), which is exactly the state the archive
// intent applies to.
type archiveFixture struct {
	db    *db.DB
	store *Store
	api   *fakeStripeCatalogAPI
}

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	return &archiveFixture{db: dbi, store: NewStore(dbi), api: newFakeStripeCatalogAPI()}
}

func (fx *archiveFixture) runnerWith(cfg ModeView) *Runner {
	handler := &StripeArchivePriceHandler{stripeArchiveCore{
		Stripe:      fx.api,
		LoadCatalog: dbCatalogRowsLoader(fx.db),
		Policy:      DefaultBackoff,
	}}
	r := &Runner{Store: fx.store, Registry: NewRegistry(handler)}
	if cfg != nil {
		r.Config = cfg
	}
	return r
}

func (fx *archiveFixture) enqueueArchive(t *testing.T, objectID string, dueAt time.Time) uuid.UUID {
	t.Helper()
	row, err := fx.store.Enqueue(context.Background(), EnqueueParams{
		MerchantID:       dbtest.TestTenantID.UUID(),
		Provider:       "stripe",
		IntentType:     TypeStripeArchivePrice,
		Payload:        StripeArchivePayload{ObjectID: objectID, MarkerKey: "retired.usd.900.30"},
		IdempotencyKey: StripeArchiveIdempotencyKey(TypeStripeArchivePrice, objectID),
		NextAttemptAt:  dueAt,
		Origin:         OriginAdmin,
		OriginReason:   "integration test (--exhaustive)",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = fx.db.Pool().Exec(context.Background(),
			"DELETE FROM openrails.provider_intents WHERE id = $1", row.ID)
	})
	return row.ID
}

func (fx *archiveFixture) intent(t *testing.T, id uuid.UUID) (status string, reason string) {
	t.Helper()
	row, err := fx.db.Gen(context.Background()).GetProviderIntent(context.Background(), id)
	require.NoError(t, err)
	if row.LastFailureReason != nil {
		reason = *row.LastFailureReason
	}
	return row.Status, reason
}

func (fx *archiveFixture) makeDue(t *testing.T, id uuid.UUID) {
	t.Helper()
	_, err := fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.provider_intents SET next_attempt_at = now() WHERE id = $1", id)
	require.NoError(t, err)
}

// TestArchiveIntentParksUnderReadonlyDrainsUnderFull pins the #358-D mode
// interplay for catalog archives: under readonly even the ADMIN-origin
// archive parks (pending, reason recorded, no provider traffic); when the
// mode lifts to full the scheduled executor drains it and the Stripe write
// carries the intent's idempotency key.
func TestArchiveIntentParksUnderReadonlyDrainsUnderFull(t *testing.T) {
	fx := newArchiveFixture(t)
	objectID := "price_arch_" + uuid.NewString()[:8]
	fx.api.prices[objectID] = &catalog.StripePrice{ID: objectID, Active: true}
	id := fx.enqueueArchive(t, objectID, time.Now().Add(-time.Minute))

	// readonly: parks, no provider traffic.
	_, err := fx.runnerWith(readonlyModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	status, reason := fx.intent(t, id)
	assert.Equal(t, StatusPending, status, "readonly parks (durable), never fails")
	assert.Contains(t, reason, "mode=readonly")
	assert.Empty(t, fx.api.priceWrites, "readonly attempts NOTHING")

	// limited: admin-origin archives EXECUTE (reactive completion of an
	// explicit human request) — but make it due first.
	fx.makeDue(t, id)
	_, err = fx.runnerWith(limitedModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	status, _ = fx.intent(t, id)
	assert.Equal(t, StatusSucceeded, status, "admin-origin archive executes under limited")

	w, ok := fx.api.priceWrites[objectID]
	require.True(t, ok, "the archive write must have been sent")
	require.NotNil(t, w.Active)
	assert.False(t, *w.Active)
	assert.Equal(t, StripeArchiveIdempotencyKey(TypeStripeArchivePrice, objectID), w.IdempotencyKey,
		"the Stripe mutation carries the intent's idempotency key")
}

// TestArchiveIntentSynchronousEnqueueAndExecute exercises the producer path
// ArchiveCatalogExtras uses: EnqueueAndExecute runs the intent inline through
// the same gate, returning the parked row under readonly (NOT an error) and
// the succeeded row under full.
func TestArchiveIntentSynchronousEnqueueAndExecute(t *testing.T) {
	fx := newArchiveFixture(t)
	objectID := "price_sync_" + uuid.NewString()[:8]
	fx.api.prices[objectID] = &catalog.StripePrice{ID: objectID, Active: true}

	params := EnqueueParams{
		MerchantID:       dbtest.TestTenantID.UUID(),
		Provider:       "stripe",
		IntentType:     TypeStripeArchivePrice,
		Payload:        StripeArchivePayload{ObjectID: objectID, MarkerKey: "retired.usd.900.30"},
		IdempotencyKey: StripeArchiveIdempotencyKey(TypeStripeArchivePrice, objectID),
		NextAttemptAt:  time.Now().Add(-time.Minute),
		Origin:         OriginAdmin,
	}
	row, err := fx.runnerWith(readonlyModeConfig()).EnqueueAndExecute(context.Background(), params)
	require.NoError(t, err, "a parked archive is not an error")
	t.Cleanup(func() {
		_, _ = fx.db.Pool().Exec(context.Background(),
			"DELETE FROM openrails.provider_intents WHERE id = $1", row.ID)
	})
	assert.Equal(t, StatusPending, row.Status)
	require.NotNil(t, row.LastFailureReason)
	assert.Contains(t, *row.LastFailureReason, "mode=readonly")
	assert.Empty(t, fx.api.priceWrites)

	// The durable row drains when the scheduled executor runs under full.
	fx.makeDue(t, row.ID)
	_, err = fx.runnerWith(fullModeConfig()).RunExecuteOnce(context.Background())
	require.NoError(t, err)
	status, _ := fx.intent(t, row.ID)
	assert.Equal(t, StatusSucceeded, status)
	assert.Len(t, fx.api.priceWrites, 1)
}

// TestArchiveIntentRelevanceSupersedesWhenObjectJoinsCatalog: between
// detection and execution the remote price gets linked by a local price row —
// the executor's relevance re-check (the same extra-ness predicate detection
// uses) supersedes the intent instead of archiving a now-wanted object.
func TestArchiveIntentRelevanceSupersedesWhenObjectJoinsCatalog(t *testing.T) {
	fx := newArchiveFixture(t)
	ctx := context.Background()
	objectID := "price_join_" + uuid.NewString()[:8]
	fx.api.prices[objectID] = &catalog.StripePrice{ID: objectID, Active: true}
	id := fx.enqueueArchive(t, objectID, time.Now().Add(-time.Minute))

	// Link the remote price to a fresh local row (it "joins" the catalog).
	productID := uuid.New()
	priceID := uuid.New()
	tenantID := dbtest.TestTenantID.UUID()
	_, err := fx.db.Pool().Exec(ctx, `INSERT INTO openrails.products (id, slug, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "join-prod-"+uuid.NewString()[:8], tenantID)
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(ctx, `INSERT INTO openrails.prices (id, product_id, amount, currency, billing_cycle_days, processors, merchant_id)
	      VALUES ($1, $2, 900, 'usd', 30, $3, $4)`, priceID, productID,
		[]byte(`{"stripe": {"price_id": "`+objectID+`"}}`), tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = fx.db.Pool().Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = fx.db.Pool().Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	_, err = fx.runnerWith(fullModeConfig()).RunExecuteOnce(ctx)
	require.NoError(t, err)

	status, reason := fx.intent(t, id)
	assert.Equal(t, StatusSuperseded, status)
	assert.Contains(t, reason, "local catalog")
	assert.Empty(t, fx.api.priceWrites, "a now-wanted object must never be archived")
}
