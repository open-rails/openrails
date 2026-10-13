//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Order numbers come from a sequence: a transaction holding its number open
// blocks no other, and concurrent takers get distinct numbers from 1001 up.
func TestOrderNumbersNeverBlock(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)

	held, release, first := make(chan struct{}), make(chan struct{}), make(chan int64, 1)
	var holder sync.WaitGroup
	holder.Add(1)
	go func() {
		defer holder.Done()
		_ = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			n, err := gen.New(tx).NextOrderNumber(ctx)
			if err != nil {
				return err
			}
			first <- n
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	taken := make(chan int64, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				n, err := gen.New(tx).NextOrderNumber(ctx)
				if err == nil {
					taken <- n
				}
				return err
			})
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("taking an order number blocked behind an open transaction")
	}
	close(release)
	holder.Wait()

	seen := map[int64]bool{<-first: true}
	require.Len(t, taken, 16)
	close(taken)
	for n := range taken {
		require.False(t, seen[n], "number %d issued twice", n)
		seen[n] = true
	}
	for n := range seen {
		require.GreaterOrEqual(t, n, int64(1001))
	}
}

// A restored merchant keeps its order numbers, and the destination's sequence
// moves past them: the next number never repeats a restored one.
func TestRestoreRaisesOrderNumberSequence(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	enroll(t, w, "nmi", embedded)
	client := w.client[embedded]
	w.settle()
	events, err := client.ListHostEvents(t.Context(), billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err := client.AcknowledgeHostEvents(t.Context(), []billing.HostEventID{event.ID})
		require.NoError(t, err)
	}
	w.stop()
	merchantID := client.MerchantID()
	var top int64
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT coalesce(max(number), 0) FROM billing.orders WHERE merchant_id = $1`), merchantID.UUID()).Scan(&top))
	require.GreaterOrEqual(t, top, int64(1001), "the enrolled purchase is numbered from 1001")

	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	var archive bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), source, merchantID, &archive))

	schema := "archive_numbers_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, engine.Migrate(t.Context(), w.pool, openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}}))
	name := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE") })
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+name+`.merchants (id, slug, status, permission_group_id) VALUES ($1, 'archive-numbers', 'active', $2)`, merchantID.UUID(), uuid.New())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, schema)
	require.NoError(t, err)
	_, err = merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)

	var restored, next int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT max(number) FROM "+name+".orders").Scan(&restored))
	require.Equal(t, top, restored, "restored orders keep their numbers")
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT nextval('"+schema+".order_number_seq')").Scan(&next))
	require.Greater(t, next, restored)
}
