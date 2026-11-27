//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// operationEntry is one row of the operations history.
type operationEntry struct {
	ID             uuid.UUID
	OccurredAt     time.Time
	Actor          string
	Source         string
	ObjectType     string
	ObjectID       string
	Action         string
	Changes        json.RawMessage
	OperationID    *uuid.UUID
	Rail           string
	PSPID          *uuid.UUID
	SubscriptionID *uuid.UUID
}

// loggedOperation is an operation entry's payload.
type loggedOperation struct {
	Version    int    `json:"version"`
	Kind       string `json:"kind"`
	Attempt    int32  `json:"attempt"`
	Origin     string `json:"origin"`
	Executor   string `json:"executor"`
	ReasonCode string `json:"reason_code"`
}

func (e operationEntry) operation(t *testing.T) loggedOperation {
	t.Helper()
	var op loggedOperation
	require.NoError(t, json.Unmarshal(e.Changes, &op))
	require.Equal(t, 1, op.Version)
	return op
}

// operationEntries is the operations history of one operation, oldest first.
func operationEntries(t *testing.T, w *world, merchant billing.MerchantID, operation uuid.UUID) []operationEntry {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), `SELECT id, occurred_at, actor, source, object_type, object_id, action, changes, operation_id, rail, psp_id, subscription_id
FROM `+pgx.Identifier{w.schema, "operation_log"}.Sanitize()+` WHERE merchant_id = $1 AND operation_id = $2 ORDER BY occurred_at, id`, merchant.UUID(), operation)
	require.NoError(t, err)
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (operationEntry, error) {
		var e operationEntry
		err := row.Scan(&e.ID, &e.OccurredAt, &e.Actor, &e.Source, &e.ObjectType, &e.ObjectID, &e.Action, &e.Changes, &e.OperationID, &e.Rail, &e.PSPID, &e.SubscriptionID)
		return e, err
	})
	require.NoError(t, err)
	return entries
}

// intentAttempt is an intent's status, claimed attempts and the attempt the
// breaker sent.
func intentAttempt(t *testing.T, w *world, id uuid.UUID) (status string, attempts int32, sent *int32) {
	t.Helper()
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT status, attempts, sent_attempt FROM "+pgx.Identifier{w.schema, "provider_intents"}.Sanitize()+" WHERE id = $1", id).
		Scan(&status, &attempts, &sent))
	return status, attempts, sent
}

func TestOperationsHistory(t *testing.T) {
	t.Run("OR-J43-S08/an-operation-is-recorded-before-its-provider-call-and-survives-a-crash", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()))
		// Stripe takes the renewal's charge; its answer is held while the
		// worker that sent it dies.
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		p.mu.Lock()
		p.responseGate = gate
		p.mu.Unlock()
		defer gate.unblock()
		queue := hostFleet(t, a.world)
		prior, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		known := map[string]bool{}
		for _, job := range prior.Jobs {
			var args map[string]string
			require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
			known[args["intent_id"]] = true
		}
		child := startWorkerProcess(t, a.world, p)
		_, err = queue.Insert(t.Context(), registeredJob{KindName: "openrails.billing_pass"}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		select {
		case <-gate.arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("the renewal never reached Stripe")
		}
		jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		var running *rivertype.JobRow
		var intent uuid.UUID
		for _, job := range jobs.Jobs {
			var args map[string]string
			require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
			if job.State == rivertype.JobStateRunning && !known[args["intent_id"]] {
				running, intent = job, uuid.MustParse(args["intent_id"])
			}
		}
		require.NotNil(t, running, "the renewal runs as a durable operation")

		// Stripe holds the charge and OpenRails has no answer: the attempt is
		// already on record, written before the call.
		held := operationEntries(t, a.world, a.alpha.MerchantID(), intent)
		require.Len(t, held, 1)
		attempting := held[0]
		require.Equal(t, "attempting", attempting.Action)
		require.Equal(t, "system", attempting.Actor, "a renewal is the system's")
		require.Equal(t, "intent", attempting.Source)
		require.Equal(t, "provider_intent", attempting.ObjectType)
		require.Equal(t, intent.String(), attempting.ObjectID)
		require.Equal(t, "stripe", attempting.Rail)
		require.NotNil(t, attempting.PSPID)
		require.NotNil(t, attempting.SubscriptionID)
		require.Equal(t, sub.ID.UUID(), *attempting.SubscriptionID)
		op := attempting.operation(t)
		_, attempts, _ := intentAttempt(t, a.world, intent)
		require.Equal(t, attempts, op.Attempt)
		require.NotEmpty(t, op.Kind)
		require.Equal(t, "system", op.Origin)

		child.kill(t)
		gate.unblock()
		require.Equal(t, held, operationEntries(t, a.world, a.alpha.MerchantID(), intent), "the entry outlives the process that wrote it")

		a.clock.Advance(20 * time.Minute)
		startWorkerProcess(t, a.world, p)
		replay, err := queue.Insert(t.Context(), replayedOperation{Payload: running.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		renewalSettled(t, a, p, sub, due)
		require.Eventually(t, func() bool {
			job, err := queue.JobGet(t.Context(), replay.Job.ID)
			if err == nil && jobWaits(job) {
				_, _ = queue.JobRetry(t.Context(), replay.Job.ID)
			}
			return err == nil && job.State == rivertype.JobStateCompleted
		}, 20*time.Second, 20*time.Millisecond, "the recovered operation completes")
		status, _, _ := intentAttempt(t, a.world, intent)
		require.Equal(t, "succeeded", status)
		after := operationEntries(t, a.world, a.alpha.MerchantID(), intent)
		require.Equal(t, attempting, after[0], "recovery only appends")
		for _, e := range after {
			require.NotEqual(t, "failed", e.Action, "the charge Stripe took is never recorded as failed")
		}
	})

	t.Run("OR-J43-S09/a-subscriptions-history-reads-its-operations-from-the-operations-history", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		renewalSettled(t, a, p, sub, due)

		rows, err := a.pool.Query(t.Context(), `SELECT id, occurred_at, actor, source, object_type, object_id, action, changes, operation_id, rail, psp_id, subscription_id
FROM `+pgx.Identifier{a.schema, "operation_log"}.Sanitize()+` WHERE merchant_id = $1 AND subscription_id = $2 ORDER BY occurred_at DESC, id DESC`, a.alpha.MerchantID().UUID(), sub.ID.UUID())
		require.NoError(t, err)
		logged, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (operationEntry, error) {
			var e operationEntry
			err := row.Scan(&e.ID, &e.OccurredAt, &e.Actor, &e.Source, &e.ObjectType, &e.ObjectID, &e.Action, &e.Changes, &e.OperationID, &e.Rail, &e.PSPID, &e.SubscriptionID)
			return e, err
		})
		require.NoError(t, err)
		phases := map[string]int{}
		for _, e := range logged {
			phases[e.Action]++
		}
		require.Positive(t, phases["attempting"], "the renewal's attempt is recorded")
		require.Positive(t, phases["succeeded"], "and its result")

		var operations []billing.SubscriptionHistoryEntry
		for page := (billing.PageRequest{Limit: 2}); ; {
			history, err := a.alpha.GetSubscriptionHistory(t.Context(), sub.ID, page)
			require.NoError(t, err)
			for _, e := range history.Events.Items {
				if e.Kind == "provider_operation" {
					operations = append(operations, e)
				}
			}
			require.Contains(t, history.Coverage.Sources, "provider_operation")
			if history.Events.Next == "" {
				break
			}
			page.Cursor = history.Events.Next
		}
		require.Len(t, operations, len(logged), "every entry of the subscription's operations, once, a page at a time")
		for i, e := range logged {
			read := operations[i]
			op := e.operation(t)
			require.Equal(t, "provider_operation:"+e.ID.String(), read.ID)
			require.True(t, read.At.Equal(e.OccurredAt))
			require.NotNil(t, read.Operation)
			require.Equal(t, e.OperationID.String(), read.Operation.ID)
			require.Equal(t, e.Action, read.Operation.Phase)
			require.Equal(t, op.Kind, read.Operation.Kind)
			require.EqualValues(t, op.Attempt, read.Operation.Attempt)
			require.Equal(t, op.ReasonCode, read.Operation.Reason)
			require.NotNil(t, read.Attribution)
			require.Equal(t, op.Origin, read.Attribution.Origin)
			require.Equal(t, op.Executor, read.Attribution.Executor)
			require.Equal(t, e.OperationID.String(), read.Attribution.OperationID)
		}
	})

	t.Run("OR-J43-S10/a-price-keys-moves-are-configuration-history-with-their-revisions", func(t *testing.T) {
		a := newAuthorityWorld(t)
		product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "tiered", DisplayName: "Tiered"})
		require.NoError(t, err)
		first, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 5_000_000, Currency: "USD", BillingIntervalMonths: ptr(1)})
		require.NoError(t, err)
		a.clock.Advance(time.Hour)
		// New terms on the key: a new version, which the key moves to.
		second, err := a.alphaRemote.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 7_000_000, Currency: "USD", BillingIntervalMonths: ptr(1)})
		require.NoError(t, err)
		require.NotEqual(t, first.ID, second.ID)
		a.clock.Advance(time.Hour)
		_, err = a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Field[bool]{Set: true, Value: true}})
		require.NoError(t, err)
		a.clock.Advance(time.Hour)
		_, err = a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Field[bool]{Set: true, Value: false}})
		require.NoError(t, err)
		current, err := a.alphaRemote.GetPrice(t.Context(), second.ID)
		require.NoError(t, err)

		entries := objectHistory(t, a.alphaRemote, billing.LogObjectPrice, "tiered:monthly")
		require.Equal(t, []billing.LogAction{billing.LogActionMove, billing.LogActionMove, billing.LogActionUpdate, billing.LogActionCreate}, actions(entries), "one entry per price change, with standalone moves for product changes")
		var moves []billing.LogEntry
		for _, e := range entries {
			if e.Action == billing.LogActionMove || changeOf(t, e).Move != nil {
				moves = append(moves, e)
			}
		}
		type move struct {
			price    billing.PriceID
			archived bool
			revision int64
		}
		var got []move
		for _, e := range moves {
			require.True(t, strings.HasPrefix(string(e.Actor), "staff:"), e.Actor)
			require.Equal(t, billing.LogSourceAPI, e.Source)
			c := changeOf(t, e)
			require.NotNil(t, c.Revision, "a move carries its key's revision")
			revision := *c.Revision
			if c.Move != nil {
				c = *c.Move
			}
			got = append(got, move{c.PriceID, c.Archived, revision})
		}
		require.Equal(t, []move{
			{second.ID, false, current.Revision}, // the product unarchived: the key names its price again
			{second.ID, true, current.Revision},  // the product archived: it names none
			{second.ID, false, second.Revision},
			{first.ID, false, first.Revision},
		}, got, "newest first")
		all := historyOf(t, a.alphaRemote, billing.LogListParams{})
		require.Contains(t, all, moves[0], "moves list with every other change")

		history, err := a.alphaRemote.ListPriceHistory(t.Context(), second.ID, billing.PageRequest{})
		require.NoError(t, err)
		require.Len(t, history.Items, len(moves), "the price's history is its key's moves")
		for i, item := range history.Items {
			require.True(t, item.EffectiveAt.Equal(moves[i].OccurredAt))
			require.Equal(t, got[i].price, item.Price.ID)
			require.Equal(t, got[i].archived, item.Archived)
		}
		paged, err := a.alphaRemote.ListPriceHistory(t.Context(), first.ID, billing.PageRequest{Limit: 3})
		require.NoError(t, err)
		require.Len(t, paged.Items, 3)
		require.NotEmpty(t, paged.Next)
		rest, err := a.alphaRemote.ListPriceHistory(t.Context(), first.ID, billing.PageRequest{Limit: 3, Cursor: paged.Next})
		require.NoError(t, err)
		require.Len(t, rest.Items, 1)
		require.Equal(t, first.ID, rest.Items[0].Price.ID, "either version reads its key's whole history")

		// Standalone movements already stored remain readable beside folded ones.
		legacy, err := json.Marshal(map[string]any{"version": 1, "revision": first.Revision, "price_id": first.ID, "archived": false})
		require.NoError(t, err)
		_, err = a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{a.schema, "change_log"}.Sanitize()+" (merchant_id, occurred_at, actor, source, object_type, object_id, action, changes) VALUES ($1, $2, 'system', 'api', 'price', 'tiered:monthly', 'move', $3)", a.alpha.MerchantID().UUID(), moves[len(moves)-1].OccurredAt.Add(-time.Hour), legacy)
		require.NoError(t, err)
		mixed, err := a.alphaRemote.ListPriceHistory(t.Context(), second.ID, billing.PageRequest{})
		require.NoError(t, err)
		require.Len(t, mixed.Items, 5)
		require.Equal(t, first.ID, mixed.Items[4].Price.ID)
		require.False(t, mixed.Items[4].Archived)
	})

	t.Run("OR-J43-S11/the-breaker-marks-an-attempt-sent-before-its-provider-call", func(t *testing.T) {
		a, journal := newNMIWorld(t)
		enableLifecycleMaintenance(t, a)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var method billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &method))
		vaults := journal.core.Vaults()
		require.Len(t, vaults, 1)

		// What the database holds while NMI has the delete.
		type observed struct {
			intent   uuid.UUID
			status   string
			attempts int32
			sent     *int32
			entries  []string
		}
		var mu sync.Mutex
		var seen []observed
		intents := pgx.Identifier{a.schema, "provider_intents"}.Sanitize()
		logged := pgx.Identifier{a.schema, "operation_log"}.Sanitize()
		journal.core.Intercept(func(r *http.Request) bool {
			return r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/v5/customers/"+vaults[0].ID)
		}, func(r *http.Request, next func() *http.Response) (*http.Response, error) {
			var o observed
			if err := a.pool.QueryRow(r.Context(), "SELECT id, status, attempts, sent_attempt FROM "+intents+" WHERE intent_type = 'nmi_vault_delete' AND payload->>'payment_method_id' = $1",
				method.ID.UUID().String()).Scan(&o.intent, &o.status, &o.attempts, &o.sent); err == nil {
				if rows, err := a.pool.Query(r.Context(), "SELECT action FROM "+logged+" WHERE merchant_id = $1 AND operation_id = $2 ORDER BY occurred_at, id", a.alpha.MerchantID().UUID(), o.intent); err == nil {
					o.entries, _ = pgx.CollectRows(rows, pgx.RowTo[string])
				}
			}
			mu.Lock()
			seen = append(seen, o)
			mu.Unlock()
			return next(), nil
		})
		status, raw = a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+method.ID.String(), a.aliceToken, nil)
		journal.core.ClearIntercepts()
		require.Equal(t, http.StatusNoContent, status, string(raw))
		require.Empty(t, journal.core.Vaults())

		mu.Lock()
		defer mu.Unlock()
		require.Len(t, seen, 1, "one delete reached NMI")
		during := seen[0]
		require.NotEqual(t, uuid.Nil, during.intent)
		require.Equal(t, "in_flight", during.status)
		require.NotNil(t, during.sent, "the breaker admitted the attempt before NMI saw it")
		require.Equal(t, during.attempts, *during.sent, "the attempt in flight is the one sent")
		require.Equal(t, []string{"attempting"}, during.entries, "its attempt was recorded ahead, its result not yet")

		entries := operationEntries(t, a.world, a.alpha.MerchantID(), during.intent)
		require.Len(t, entries, 2)
		require.Equal(t, []string{"attempting", "succeeded"}, []string{entries[0].Action, entries[1].Action})
		for _, e := range entries {
			require.True(t, strings.HasPrefix(e.Actor, "customer:"), "a customer's own card removal is the customer's: %s", e.Actor)
			require.Equal(t, "nmi", e.Rail)
			require.Equal(t, psps.Items[0].ID.UUID(), *e.PSPID)
			op := e.operation(t)
			require.Equal(t, "nmi_vault_delete", op.Kind)
			require.Equal(t, *during.sent, op.Attempt)
			require.Equal(t, "user", op.Origin)
		}
	})

	t.Run("OR-J43-S12/a-price-migrations-source-archive-waits-for-the-catalog-lock", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		target := migrationPrice(t, a, sub, "held-target", 30_000_000)
		// Another catalog writer holds the catalog lock.
		holder, err := a.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = holder.Rollback(context.Background()) }()
		var revision int64
		require.NoError(t, holder.QueryRow(t.Context(), "SELECT catalog_revision FROM "+pgx.Identifier{a.schema, "merchants"}.Sanitize()+" WHERE id = $1 FOR UPDATE",
			a.alpha.MerchantID().UUID()).Scan(&revision))
		done := make(chan error, 1)
		go func() {
			_, err := a.alphaRemote.CreatePriceMigration(context.Background(), billing.CreatePriceMigrationParams{FromPriceID: sub.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true})
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("the migration archived its source under another writer's catalog lock: %v", err)
		case <-time.After(2 * time.Second):
		}
		source, err := a.alpha.GetPrice(t.Context(), sub.PriceID)
		require.NoError(t, err)
		require.False(t, source.Archived, "the source sells until the lock is free")
		require.NoError(t, holder.Rollback(t.Context()))
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Fatal("the migration never finished once the lock was free")
		}
		source, err = a.alpha.GetPrice(t.Context(), sub.PriceID)
		require.NoError(t, err)
		require.True(t, source.Archived)

		entries := objectHistory(t, a.alphaRemote, billing.LogObjectPrice, "membership:membership-day")
		var archives, retired []billing.LogEntry
		for _, e := range entries {
			if e.Action == billing.LogActionArchive {
				archives = append(archives, e)
			}
			if c := changeOf(t, e); c.Move != nil && c.Move.Archived {
				retired = append(retired, e)
			}
		}
		require.Len(t, archives, 1, "the archive is recorded once")
		require.Len(t, retired, 1, "and the key's move to nothing once")
		require.Equal(t, archives[0], retired[0], "the archive carries its move")
		require.Equal(t, billing.LogActionArchive, entries[0].Action, "the archive is the migration's latest")
		for _, e := range []billing.LogEntry{archives[0], retired[0]} {
			require.True(t, strings.HasPrefix(string(e.Actor), "staff:"), "the migration's own actor: %s", e.Actor)
			require.EqualValues(t, source.Revision, *changeOf(t, e).Revision)
		}
		require.Equal(t, source.ID, changeOf(t, retired[0]).Move.PriceID)
	})

	t.Run("OR-J43-S13/a-parked-or-stale-claim-never-counts-as-sent", func(t *testing.T) {
		a, journal := newNMIWorld(t)
		enableLifecycleMaintenance(t, a)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: billedTo("US", "10001")})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var method billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &method))
		vaults := journal.core.Vaults()
		require.Len(t, vaults, 1)
		// NMI holds the delete the breaker admitted.
		arrived, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		journal.core.Intercept(func(r *http.Request) bool {
			return r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/v5/customers/"+vaults[0].ID)
		}, func(r *http.Request, next func() *http.Response) (*http.Response, error) {
			once.Do(func() { close(arrived) })
			<-release
			return next(), nil
		})
		defer journal.core.ClearIntercepts()
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			_, _, _ = a.do(context.Background(), http.MethodDelete, "/v1/me/payment-methods/"+method.ID.String(), a.aliceToken, nil)
		}()
		select {
		case <-arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("the delete never reached NMI")
		}
		var intent uuid.UUID
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+
			" WHERE intent_type = 'nmi_vault_delete' AND payload->>'payment_method_id' = $1", method.ID.UUID().String()).Scan(&intent))
		state, attempts, sent := intentAttempt(t, a.world, intent)
		require.Equal(t, "in_flight", state)
		require.NotNil(t, sent)
		require.Equal(t, attempts, *sent)

		// The queries the breaker and the runner run: an executor whose claim
		// is gone marks nothing, and a park of the admitted attempt, as the
		// runner parks one whose operation log is unavailable, clears the mark.
		store, err := producerDB(a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		q := store.Gen(ctx)
		stale, err := q.MarkProviderIntentSent(ctx, gen.MarkProviderIntentSentParams{MerchantID: a.alpha.MerchantID().UUID(), ID: intent, Attempt: attempts + 1})
		require.NoError(t, err)
		require.Zero(t, stale, "a claim of another attempt marks nothing")
		parked, err := q.ParkProviderIntent(ctx, gen.ParkProviderIntentParams{MerchantID: a.alpha.MerchantID().UUID(), ID: intent, NextAttemptAt: a.clock.Now().Add(time.Hour), Reason: ptr("operation log unavailable")})
		require.NoError(t, err)
		require.EqualValues(t, 1, parked)
		state, after, sent := intentAttempt(t, a.world, intent)
		require.Equal(t, "pending", state)
		require.Equal(t, attempts-1, after, "a park refunds the attempt")
		require.Nil(t, sent, "so its next claim of the same attempt is not counted as sent")

		unblock()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Fatal("the held delete never returned")
		}
	})
}
