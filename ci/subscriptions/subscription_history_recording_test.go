//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/history"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func TestSubscriptionHistoryCardChanges(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	e.c.setDefault(e.method)
	e.c.setOwnCard(e.sub, "")
	other := e.c.saveCard("nmi", visa)
	e.c.setDefault(other)
	e.c.setDefault(other) // replay must not manufacture another move
	page, err := w.client[remote].GetSubscriptionHistory(t.Context(), e.sub, billing.PageRequest{Limit: 500})
	require.NoError(t, err)
	var changes []billing.SubscriptionHistoryEntry
	for _, event := range page.Events.Items {
		if event.Change != nil && event.Change.Kind == "default_card" {
			changes = append(changes, event)
		}
	}
	require.Len(t, changes, 1)
	require.Equal(t, e.method, changes[0].Change.From.PaymentMethodID.String())
	require.Equal(t, other, changes[0].Change.To.PaymentMethodID.String())
	require.Equal(t, "user", changes[0].Attribution.Origin)
	require.NotEmpty(t, changes[0].Attribution.Actor)
	require.Equal(t, "request", changes[0].Attribution.Executor)

	// Once it selects its own card, changing the default no longer moves it.
	e.c.setOwnCard(e.sub, e.method)
	e.c.setDefault(e.method)
	page, err = w.client[embedded].GetSubscriptionHistory(t.Context(), e.sub, billing.PageRequest{Limit: 500})
	require.NoError(t, err)
	count := 0
	for _, event := range page.Events.Items {
		if event.Change != nil && event.Change.Kind == "default_card" {
			count++
		}
	}
	require.Equal(t, 1, count)
	e.c.setOwnCard(e.sub, "")
	w.settle()
	w.stop()
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	method, err := billing.ParsePaymentMethodID(e.method)
	require.NoError(t, err)
	ev := paymentmethods.CardEvent{MerchantID: mid.UUID(), PaymentMethodID: method.UUID(), Source: paymentmethods.SourceNMIACU, EventRef: "history-reissue", At: w.clock.Now(), Card: paymentmethods.Card{Card: models.Card{ExpYear: 2040, ExpMonth: 12}}}
	// The updater's version and subscription association share one transaction.
	apply := func(rollback bool) error {
		return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := paymentmethods.ApplyCardLifecycle(ctx, d.NewWithPgxTx(tx).Gen(ctx), ev)
			if err != nil {
				return err
			}
			if rollback {
				return errors.New("rollback")
			}
			return nil
		})
	}
	require.ErrorContains(t, apply(true), "rollback")
	rows, err := d.Gen(ctx).ListSubscriptionHistory(ctx, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: e.sub.UUID(), RowLimit: 500})
	require.NoError(t, err)
	for _, row := range rows {
		require.NotEqual(t, "card_version", row.Kind)
	}
	require.NoError(t, apply(false))
	require.NoError(t, apply(false))
	rows, err = d.Gen(ctx).ListSubscriptionHistory(ctx, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: e.sub.UUID(), RowLimit: 500})
	require.NoError(t, err)
	count = 0
	for _, row := range rows {
		if row.Source == 4 && row.Kind == "card_version" {
			count++
			require.Equal(t, "nmi_acu", row.AttributionOrigin)
			require.NotNil(t, row.MethodVersionID)
			require.Equal(t, method.UUID(), *row.ToPaymentMethodID)
		}
	}
	require.Equal(t, 1, count)
	ev.EventRef = "history-closed"
	ev.Advice = paymentmethods.AdviceClosed
	require.NoError(t, apply(false))
	rows, err = d.Gen(ctx).ListSubscriptionHistory(ctx, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: e.sub.UUID(), RowLimit: 500})
	require.NoError(t, err)
	closed := 0
	for _, row := range rows {
		if row.Source == 4 && row.Kind == "card_version" && row.Reason == "closed" {
			closed++
			require.Equal(t, "nmi_acu", row.AttributionOrigin)
		}
	}
	require.Equal(t, 1, closed, "card closure must retain the association before the default is cleared")
	versions, err := d.Gen(ctx).ListPaymentMethodVersions(ctx, gen.ListPaymentMethodVersionsParams{MerchantID: mid.UUID(), PaymentMethodID: method.UUID()})
	require.NoError(t, err)
	require.Equal(t, "closed", versions[len(versions)-1].Kind, "same-time observations retain database UUID ordering used by reconciliation")
}

func TestSubscriptionHistoryRetryAtomicityAndAttribution(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	other := enroll(t, w, "nmi", embedded)
	w.settle()
	w.stop()
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	mid := w.client[embedded].MerchantID()
	base := merchant.WithID(t.Context(), mid)
	operation := uuid.New()
	ctx := history.WithAttribution(base, history.Attribution{Origin: "admin", Actor: "original-operator", Executor: "verification", OperationID: &operation})
	read := func() []gen.ListSubscriptionHistoryRow {
		rows, err := d.Gen(base).ListSubscriptionHistory(base, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: e.sub.UUID(), RowLimit: 500})
		require.NoError(t, err)
		return rows
	}
	before := len(read())
	at := w.clock.Now().Add(time.Hour)
	write := func(rollback bool) error {
		return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			repo := subscriptions.NewSubscriptionRepo(d.NewWithPgxTx(tx))
			sub, err := repo.GetByIDForUpdate(ctx, e.sub.UUID())
			if err != nil {
				return err
			}
			sub.NextRetryAt = &at
			sub.RetryAttempts = new(2)
			if err = repo.UpdateAt(ctx, sub, at); err != nil {
				return err
			}
			if rollback {
				return errors.New("rollback")
			}
			// A direct write of the same row and a different row cannot borrow metadata.
			if _, err = tx.Exec(ctx, w.q(`UPDATE billing.subscriptions SET transient_retries=transient_retries+1 WHERE merchant_id=$1 AND id=$2`), mid.UUID(), sub.ID); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, w.q(`UPDATE billing.subscriptions SET transient_retries=transient_retries+1 WHERE merchant_id=$1 AND id=$2`), mid.UUID(), other.sub.UUID())
			return err
		})
	}
	require.ErrorContains(t, write(true), "rollback")
	require.Len(t, read(), before)
	require.NoError(t, write(false))
	rows := read()
	require.Len(t, rows, before+2)
	attributed, unknown := 0, 0
	for _, row := range rows {
		if row.Source != 4 {
			continue
		}
		if row.Actor == "original-operator" {
			attributed++
			require.Equal(t, "verification", row.Executor)
			require.Equal(t, operation, *row.OperationID)
			require.Equal(t, int32(2), *row.ToRetryAttempts)
			require.True(t, at.Equal(*row.ToNextRetryAt))
		} else {
			unknown++
			require.Empty(t, row.Actor)
			require.Nil(t, row.OperationID)
		}
	}
	require.Equal(t, 1, attributed)
	require.Equal(t, 1, unknown)
	otherRows, err := d.Gen(base).ListSubscriptionHistory(base, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: other.sub.UUID(), RowLimit: 500})
	require.NoError(t, err)
	for _, row := range otherRows {
		if row.Source == 4 {
			require.Empty(t, row.Actor)
			require.Nil(t, row.OperationID)
		}
	}
	// Two locked updates retain both old/new transitions; the same image is a no-op.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			errs <- d.MerchantTx(base, func(ctx context.Context, tx pgx.Tx) error {
				repo := subscriptions.NewSubscriptionRepo(d.NewWithPgxTx(tx))
				sub, err := repo.GetByIDForUpdate(ctx, e.sub.UUID())
				if err != nil {
					return err
				}
				sub.TransientRetries++
				return repo.UpdateAt(ctx, sub, at)
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Len(t, read(), before+4)
	repo := subscriptions.NewSubscriptionRepo(d)
	sub, err := repo.GetByID(base, e.sub.UUID())
	require.NoError(t, err)
	require.NoError(t, repo.UpdateAt(base, sub, at))
	require.Len(t, read(), before+4)
	// Expired evidence is deleted only through a declared, bounded sweep.
	_, err = w.pool.Exec(base, w.q(`INSERT INTO billing.subscription_billing_changes (merchant_id,subscription_id,kind,occurred_at)
 VALUES ($1,$2,'retry_schedule',now()-interval '762 days'),($1,$2,'retry_schedule',now()-interval '762 days')`), mid.UUID(), e.sub.UUID())
	require.NoError(t, err)
	require.NoError(t, d.MerchantTx(base, func(ctx context.Context, tx pgx.Tx) error {
		q := d.NewWithPgxTx(tx).Gen(ctx)
		if err := q.DeclareRetentionSweep(ctx, "subscription_billing_changes"); err != nil {
			return err
		}
		n, err := q.DeleteSubscriptionBillingChangesPastRetention(ctx, gen.DeleteSubscriptionBillingChangesPastRetentionParams{MerchantID: mid.UUID(), RetentionDays: 761, RowLimit: 1})
		require.Equal(t, int64(1), n)
		return err
	}))
	require.Len(t, read(), before+5)
	// Append-only facts cannot be edited or removed early.
	_, err = w.pool.Exec(base, w.q(`UPDATE billing.subscription_billing_changes SET actor='rewritten' WHERE subscription_id=$1`), e.sub.UUID())
	require.Error(t, err)
	_, err = w.pool.Exec(base, w.q(`DELETE FROM billing.subscription_billing_changes WHERE subscription_id=$1`), e.sub.UUID())
	require.Error(t, err)
}

func TestSubscriptionHistoryHeldOperation(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	w.settle()
	jobs := w.jobs
	w.stop()
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	op := uuid.New()
	now := w.clock.Now()
	_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.provider_intents
 (merchant_id,id,rail,psp_id,intent_type,subscription_id,idempotency_key,status,next_attempt_at,origin,actor)
 SELECT merchant_id,$2,rail,psp_id,'manual_rebill',id,($2::uuid)::text,'pending',$3,'user','original-customer'
 FROM billing.subscriptions WHERE id=$1`), e.sub.UUID(), op, now)
	require.NoError(t, err)
	d.SetRiverJobInserter(jobs)
	store := intents.NewStore(d)
	// No handler: a real runner records the pre-dispatch hold without a charge.
	runner := &intents.Runner{Store: store, Logger: store, Registry: intents.NewRegistry(), Clock: w.clock}
	for i := 0; i < 2; i++ {
		_, err = runner.ExecuteByID(ctx, op)
		require.NoError(t, err)
		w.clock.Advance(6 * time.Minute)
	}
	held := func() []gen.ListSubscriptionHistoryRow {
		rows, err := d.Gen(ctx).ListSubscriptionHistory(ctx, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: e.sub.UUID(), RowLimit: 500})
		require.NoError(t, err)
		var out []gen.ListSubscriptionHistoryRow
		for _, row := range rows {
			if row.OperationID != nil && *row.OperationID == op {
				out = append(out, row)
			}
		}
		return out
	}
	rows := held()
	require.Len(t, rows, 1)
	require.Equal(t, "handler_unavailable", rows[0].Reason)
	require.Equal(t, "original-customer", rows[0].Actor)
	require.Equal(t, "execution", rows[0].Executor)
	// Audit failure/rollback must roll back the transition too.
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		local := intents.NewStore(d.NewWithPgxTx(tx))
		if err := local.Park(ctx, op, now.Add(time.Hour), "another hold"); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	require.ErrorContains(t, err, "rollback")
	require.Len(t, held(), 1)
	_, claimed, err := store.ClaimByID(ctx, op, w.clock.Now(), w.clock.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	// Concurrent observations of the same new hold serialize on the intent.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Go(func() { errs <- store.Park(ctx, op, now, "still held") })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Len(t, held(), 2)
	// A new execution attempt separates two hold episodes, without claiming an unpause.
	require.NoError(t, store.LogExternalMutation(ctx, intents.MutationLogParams{MerchantID: mid.UUID(), Provider: "stripe", PspID: w.psp["stripe"].UUID(), ProviderIntentID: &op, IntentType: "manual_rebill", Phase: intents.MutationLogPhaseAttempting}))
	_, err = runner.ExecuteByID(ctx, op)
	require.NoError(t, err)
	require.Len(t, held(), 4)
	// Retention may remove a completed provider command; its newer log remains
	// scoped to this subscription and retains its original operation reference.
	retired := uuid.New()
	_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.provider_intents
 (merchant_id,id,rail,psp_id,intent_type,subscription_id,idempotency_key,status,next_attempt_at,origin,actor,created_at,updated_at,executed_at)
 SELECT merchant_id,$2,rail,psp_id,'stripe_cancel_subscription',id,($2::uuid)::text,'succeeded',now()-interval '762 days','admin','retired-operator',now()-interval '762 days',now()-interval '762 days',now()-interval '762 days'
 FROM billing.subscriptions WHERE id=$1`), e.sub.UUID(), retired)
	require.NoError(t, err)
	require.NoError(t, store.LogExternalMutation(ctx, intents.MutationLogParams{MerchantID: mid.UUID(), Provider: "stripe", PspID: w.psp["stripe"].UUID(), ProviderIntentID: &retired, IntentType: "stripe_cancel_subscription", Phase: intents.MutationLogPhaseSucceeded}))
	_, err = d.Gen(ctx).DeleteFinishedOutboxIntentsBefore(ctx, gen.DeleteFinishedOutboxIntentsBeforeParams{MerchantID: mid.UUID(), Cutoff: time.Now().Add(-761 * 24 * time.Hour), RowLimit: 100})
	require.NoError(t, err)
	_, err = store.Get(ctx, retired)
	require.ErrorIs(t, err, pgx.ErrNoRows)
	retained, err := d.Gen(ctx).ListSubscriptionHistory(ctx, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: e.sub.UUID(), RowLimit: 500})
	require.NoError(t, err)
	found := 0
	for _, row := range retained {
		if row.OperationID != nil && *row.OperationID == retired {
			found++
			require.Equal(t, "retired-operator", row.Actor)
		}
	}
	require.Equal(t, 1, found)
}

// The write-posture path retains an unsubmitted collection as unknown for
// readback. It still records a hold, not an issuer decline or a retry promise.
func TestSubscriptionHistoryWritePostureHold(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	w.settle()
	jobs := w.jobs
	w.stop()
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	op := uuid.New()
	_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.provider_intents
 (merchant_id,id,rail,psp_id,intent_type,subscription_id,idempotency_key,status,next_attempt_at,origin,actor)
 SELECT merchant_id,$2,rail,psp_id,'subscription_collection',id,($2::uuid)::text,'pending',$3,'user','original-customer'
 FROM billing.subscriptions WHERE id=$1`), e.sub.UUID(), op, w.clock.Now())
	require.NoError(t, err)
	d.SetRiverJobInserter(jobs)
	store := intents.NewStore(d)
	h := &historyHeldHandler{t: t}
	runner := &intents.Runner{Store: store, Logger: store, Registry: intents.NewRegistry(h), Clock: w.clock}
	row, err := runner.ExecuteByID(ctx, op)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
	for i := 0; i < 2; i++ {
		w.clock.Advance(6 * time.Minute)
		_, err = runner.VerifyByID(ctx, op)
		require.NoError(t, err)
	}
	rows, err := d.Gen(ctx).ListSubscriptionHistory(ctx, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: e.sub.UUID(), RowLimit: 500})
	require.NoError(t, err)
	holds := 0
	for _, row := range rows {
		if row.OperationID == nil || *row.OperationID != op {
			continue
		}
		require.Equal(t, "original-customer", row.Actor)
		if row.Category == "parked" {
			holds++
			require.Equal(t, "write_posture", row.Reason)
		} else {
			require.Equal(t, "unknown", row.Category)
			require.Equal(t, "verification", row.Executor)
		}
	}
	require.Equal(t, 1, holds)
}

type historyHeldHandler struct{ t *testing.T }

func (*historyHeldHandler) Type() string { return "subscription_collection" }
func (*historyHeldHandler) CheckRelevance(context.Context, gen.BillingProviderIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *historyHeldHandler) Execute(context.Context, gen.BillingProviderIntent) intents.Outcome {
	h.t.Error("held execution reached provider")
	return intents.Ambiguous("unexpected")
}
func (*historyHeldHandler) Verify(context.Context, gen.BillingProviderIntent) intents.Outcome {
	return intents.Retryable("verified unsubmitted")
}
func (*historyHeldHandler) Backoff(int32) time.Duration { return time.Minute }

func TestSubscriptionHistoryArchiveRoundTrip(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	e.c.setDefault(e.method)
	e.c.setOwnCard(e.sub, "")
	other := e.c.saveCard("nmi", visa)
	e.c.setDefault(other)
	w.settle()
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	for {
		events, err := w.client[embedded].ListHostEvents(ctx, billing.HostEventListParams{})
		require.NoError(t, err)
		if len(events.Items) == 0 {
			break
		}
		for _, event := range events.Items {
			_, err = w.client[embedded].AcknowledgeHostEvents(ctx, []billing.HostEventID{event.ID})
			require.NoError(t, err)
		}
	}
	w.stop()
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	mid := w.client[embedded].MerchantID()
	var archive bytes.Buffer
	require.NoError(t, merchantarchive.Export(ctx, d, mid, &archive))
	target := "history_restore_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, engine.Migrate(ctx, w.pool, openrails.Config{Database: openrails.DatabaseConfig{Schema: target, RiverSchema: target}}))
	quoted := pgx.Identifier{target}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	_, err = w.pool.Exec(ctx, "INSERT INTO "+quoted+`.merchants (id,slug,status,permission_group_id) VALUES ($1,'history-restored','active',$2)`, mid.UUID(), uuid.New())
	require.NoError(t, err)
	dest, err := db.NewWithPGXPool(w.pool, target)
	require.NoError(t, err)
	_, err = merchantarchive.Restore(ctx, dest, mid, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	for _, table := range []string{"subscription_status_transitions", "subscription_billing_changes"} {
		read := func(schema string) string {
			var value string
			require.NoError(t, w.pool.QueryRow(ctx, fmt.Sprintf("SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM %s.%s t WHERE merchant_id=$1", pgx.Identifier{schema}.Sanitize(), table), mid.UUID()).Scan(&value))
			return value
		}
		require.Equal(t, read(w.schema), read(target), "restore must preserve original events and attribution without inventing new ones: %s", table)
	}
	replay, err := merchantarchive.Restore(ctx, dest, mid, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
}

func TestSubscriptionHistoryStripeDetachBatchAttribution(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	price := w.membership("history-second", 1000000)
	second := e.c.subscribeAgain(embedded, "stripe", price.ID.String(), "history-second", e.method)
	w.settle()
	w.stop()
	d, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	method, err := billing.ParsePaymentMethodID(e.method)
	require.NoError(t, err)
	n, err := d.Gen(ctx).ClearStripePaymentMethodSubscriptions(ctx, gen.ClearStripePaymentMethodSubscriptionsParams{MerchantID: mid.UUID(), PspID: w.psp["stripe"].UUID(), PaymentMethodID: method.UUID()})
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
	for _, sub := range []billing.SubscriptionID{e.sub, second} {
		rows, err := d.Gen(ctx).ListSubscriptionHistory(ctx, gen.ListSubscriptionHistoryParams{MerchantID: mid.UUID(), SubscriptionID: sub.UUID(), RowLimit: 500})
		require.NoError(t, err)
		found := 0
		for _, row := range rows {
			if row.Source == 4 && row.Kind == "card_selection" {
				found++
				require.Equal(t, "provider_card_detached", row.Reason)
				require.Equal(t, "provider", row.AttributionOrigin)
				require.Equal(t, "stripe_observation", row.Executor)
				require.Nil(t, row.ToPaymentMethodID)
			}
		}
		require.Equal(t, 1, found)
	}
}
