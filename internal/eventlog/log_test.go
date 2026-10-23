package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/stretchr/testify/require"
)

func errorCode(err error) string {
	var typed *apperr.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

type fixture struct {
	pool     *pgxpool.Pool
	database *db.DB
	schema   string
}

func (f fixture) sql(s string) string { return strings.ReplaceAll(s, "billing.", f.schema+".") }
func (f fixture) exec(t *testing.T, s string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), f.sql(s), args...)
	require.NoError(t, err)
}
func (f fixture) merchant(t *testing.T) (billing.MerchantID, uuid.UUID) {
	t.Helper()
	mid, customer := uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO billing.merchants(id) VALUES($1)`, mid)
	f.exec(t, `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
	return billing.MerchantID(mid), customer
}
func (f fixture) insert(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, mid billing.MerchantID, customer uuid.UUID, kind string) (uuid.UUID, error) {
	id := uuid.New()
	_, err := q.Exec(ctx, f.sql(`INSERT INTO billing.notification_events(id,merchant_id,customer_id,event_type,data) VALUES($1,$2,$3,$4,'{}')`), id, mid.UUID(), customer, kind)
	return id, err
}
func (f fixture) add(t *testing.T, mid billing.MerchantID, customer uuid.UUID, kind string) uuid.UUID {
	t.Helper()
	id, err := f.insert(t.Context(), f.pool, mid, customer, kind)
	require.NoError(t, err)
	return id
}
func (f fixture) list(t *testing.T, mid billing.MerchantID, q billing.EventQuery) billing.EventPage {
	t.Helper()
	p, err := List(t.Context(), f.database, mid, q, DefaultRetention, time.Now())
	require.NoError(t, err)
	return p
}
func (f fixture) pendingEmpty(t *testing.T) {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(t.Context(), f.sql(`SELECT count(*) FROM billing.event_log_pending`)).Scan(&n))
	require.Zero(t, n)
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := fixture{pool: pool, schema: "eventlog_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	f.exec(t, `CREATE SCHEMA `+pgx.Identifier{f.schema}.Sanitize())
	f.exec(t, `CREATE FUNCTION `+f.schema+`.billing_restore_active(uuid) RETURNS boolean LANGUAGE sql AS $$ SELECT COALESCE(current_setting('test.billing_restore',true),'')='true' $$`)
	f.exec(t, `CREATE TABLE billing.merchants(id uuid PRIMARY KEY);
CREATE TABLE billing.customers(merchant_id uuid REFERENCES billing.merchants,id uuid,issuer text,PRIMARY KEY(merchant_id,id));
CREATE TABLE billing.payments(merchant_id uuid,id uuid,customer_id uuid,subscription_id uuid,order_id uuid);
CREATE TABLE billing.host_outbox(id uuid,merchant_id uuid,event_type text,subject_type text,payment_id uuid,amount bigint,subject_id uuid,currency text,occurred_at timestamptz DEFAULT now(),data jsonb DEFAULT '{}');
CREATE TABLE billing.notification_events(id uuid,merchant_id uuid,customer_id uuid,event_type text,data jsonb DEFAULT '{}',created_at timestamptz DEFAULT now(),inserted_at timestamptz DEFAULT now(),PRIMARY KEY(merchant_id,id),FOREIGN KEY(merchant_id,customer_id) REFERENCES billing.customers ON DELETE CASCADE);`)

	seedMid, seedCustomer, seedEvent := uuid.New(), uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO billing.merchants VALUES($1)`, seedMid)
	f.exec(t, `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, seedMid, seedCustomer)
	f.exec(t, `INSERT INTO billing.notification_events(id,merchant_id,customer_id,event_type) VALUES($1,$2,$3,'before_migration')`, seedEvent, seedMid, seedCustomer)
	sql, err := os.ReadFile("../migrate/postgres/0110_retained_event_log.up.sql")
	require.NoError(t, err)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), f.sql(string(sql)))
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
	f.database, err = db.NewWithPGXPool(pool, f.schema)
	require.NoError(t, err)
	seeded := f.list(t, billing.MerchantID(seedMid), billing.EventQuery{})
	require.Len(t, seeded.Events, 1)
	require.Equal(t, "ntf_"+seedEvent.String(), seeded.Events[0].ID)
	return f
}

func TestEventLog(t *testing.T) {
	f := newFixture(t)
	t.Run("host domain account identity is preserved", func(t *testing.T) {
		mid, customer := f.merchant(t)
		account := uuid.New()
		payload, err := json.Marshal(map[string]any{"account_id": account, "method": "connect"})
		require.NoError(t, err)
		f.exec(t, `INSERT INTO billing.host_outbox(id,merchant_id,event_type,subject_type,subject_id,data) VALUES($1,$2,'customer.network_linked','customer',$3,$4)`, uuid.New(), mid.UUID(), customer, payload)
		page := f.list(t, mid, billing.EventQuery{})
		require.Len(t, page.Events, 1)
		var got billing.CustomerNetworkHostEvent
		require.NoError(t, json.Unmarshal(page.Events[0].Data, &got))
		require.Equal(t, account, got.AccountID.UUID())
		require.Equal(t, customer, got.CustomerID.UUID())
	})
	t.Run("readers are independent and source deletion keeps account snapshot", func(t *testing.T) {
		mid, customer := f.merchant(t)
		empty := f.list(t, mid, billing.EventQuery{})
		require.Empty(t, empty.Events)
		require.NotEmpty(t, empty.NextCursor)
		first := f.add(t, mid, customer, "premium_ended")
		a := f.list(t, mid, billing.EventQuery{After: empty.NextCursor})
		b := f.list(t, mid, billing.EventQuery{After: empty.NextCursor})
		require.Equal(t, a, b)
		require.Len(t, a.Events, 1)
		require.Equal(t, "ntf_"+first.String(), a.Events[0].ID)
		f.exec(t, `SELECT billing.queue_event_log(billing.notification_event_record(n)) FROM billing.notification_events n WHERE merchant_id=$1`, mid.UUID())
		require.Len(t, f.list(t, mid, billing.EventQuery{}).Events, 1, "replayed source IDs do not append twice")
		f.exec(t, `DELETE FROM billing.customers WHERE merchant_id=$1 AND id=$2`, mid.UUID(), customer)
		replay := f.list(t, mid, billing.EventQuery{})
		require.Len(t, replay.Events, 1)
		var data map[string]any
		require.NoError(t, json.Unmarshal(replay.Events[0].Data, &data))
		require.Equal(t, customer.String(), data["account_id"])
		require.Equal(t, "", data["account_issuer"])
		require.Equal(t, customer.String(), data["customer_id"])
		require.Equal(t, customer.String(), replay.Events[0].Subject.ID)
		f.pendingEmpty(t)
	})
	t.Run("commit ordering never skips a late transaction", func(t *testing.T) {
		mid, customer := f.merchant(t)
		before := f.list(t, mid, billing.EventQuery{})
		slow, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer slow.Rollback(t.Context())
		late, err := f.insert(t.Context(), slow, mid, customer, "late")
		require.NoError(t, err)
		rolled, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		_, err = f.insert(t.Context(), rolled, mid, customer, "rollback")
		require.NoError(t, err)
		require.NoError(t, rolled.Rollback(t.Context()))
		early := f.add(t, mid, customer, "early")
		p := f.list(t, mid, billing.EventQuery{After: before.NextCursor})
		require.Len(t, p.Events, 1)
		require.Equal(t, "ntf_"+early.String(), p.Events[0].ID)
		require.NoError(t, slow.Commit(t.Context()))
		next := f.list(t, mid, billing.EventQuery{After: p.NextCursor})
		require.Len(t, next.Events, 1)
		require.Equal(t, "ntf_"+late.String(), next.Events[0].ID)
		f.pendingEmpty(t)
	})
	t.Run("opposite merchant statement order cannot deadlock flush", func(t *testing.T) {
		a, ca := f.merchant(t)
		b, cb := f.merchant(t)
		x, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer x.Rollback(t.Context())
		y, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer y.Rollback(t.Context())
		for _, entry := range []struct {
			tx  pgx.Tx
			mid billing.MerchantID
			c   uuid.UUID
		}{{x, a, ca}, {x, b, cb}, {y, b, cb}, {y, a, ca}} {
			_, err = f.insert(t.Context(), entry.tx, entry.mid, entry.c, "cross")
			require.NoError(t, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		done := make(chan error, 2)
		go func() { done <- x.Commit(ctx) }()
		go func() { done <- y.Commit(ctx) }()
		require.NoError(t, <-done)
		require.NoError(t, <-done)
		require.Len(t, f.list(t, a, billing.EventQuery{}).Events, 2)
		require.Len(t, f.list(t, b, billing.EventQuery{}).Events, 2)
		f.pendingEmpty(t)
	})
	t.Run("pagination filters and foreign cursors", func(t *testing.T) {
		mid, customer := f.merchant(t)
		other, _ := f.merchant(t)
		f.add(t, mid, customer, "alpha")
		f.add(t, mid, customer, "beta")
		f.add(t, mid, customer, "alpha")
		first := f.list(t, mid, billing.EventQuery{Limit: 1, Types: []string{"alpha", "alpha"}})
		require.True(t, first.HasMore)
		require.Len(t, first.Events, 1)
		next := f.list(t, mid, billing.EventQuery{After: first.NextCursor, Limit: 1, Types: []string{"alpha"}})
		require.False(t, next.HasMore)
		require.Len(t, next.Events, 1)
		_, err := List(t.Context(), f.database, other, billing.EventQuery{After: first.NextCursor, Types: []string{"alpha"}}, DefaultRetention, time.Now())
		require.Equal(t, billing.CodeInvalidCursor, errorCode(err))
		_, err = List(t.Context(), f.database, mid, billing.EventQuery{After: first.NextCursor, Types: []string{"beta"}}, DefaultRetention, time.Now())
		require.Equal(t, billing.CodeInvalidCursor, errorCode(err))
		_, err = List(t.Context(), f.database, mid, billing.EventQuery{After: "nonsense"}, DefaultRetention, time.Now())
		require.Equal(t, billing.CodeInvalidCursor, errorCode(err))
	})
	t.Run("retention reset and alternate history expire explicitly", func(t *testing.T) {
		mid, c := f.merchant(t)
		f.add(t, mid, c, "a")
		f.add(t, mid, c, "b")
		p := f.list(t, mid, billing.EventQuery{Limit: 1})
		future := time.Now().Add(2 * DefaultRetention)
		_, err := List(t.Context(), f.database, mid, billing.EventQuery{After: p.NextCursor}, DefaultRetention, future)
		require.Equal(t, billing.CodeEventCursorExpired, errorCode(err))
		blank, err := List(t.Context(), f.database, mid, billing.EventQuery{}, DefaultRetention, future)
		require.NoError(t, err)
		require.Empty(t, blank.Events)
		removed, err := Prune(t.Context(), f.database, future, DefaultRetention, 1)
		require.NoError(t, err)
		require.Equal(t, 1, removed)
		// The cursor at the virtual floor is valid after physical pruning too.
		_, err = List(t.Context(), f.database, mid, billing.EventQuery{After: blank.NextCursor}, DefaultRetention, future)
		require.NoError(t, err)
		f.exec(t, `UPDATE billing.event_log_streams SET epoch=$2 WHERE merchant_id=$1`, mid.UUID(), uuid.New())
		_, err = List(t.Context(), f.database, mid, billing.EventQuery{After: blank.NextCursor}, DefaultRetention, time.Now())
		require.Equal(t, billing.CodeEventCursorExpired, errorCode(err))
	})
	t.Run("pinned merchant cannot append or read another merchant", func(t *testing.T) {
		a, _ := f.merchant(t)
		b, customer := f.merchant(t)
		tx, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer tx.Rollback(t.Context())
		_, err = tx.Exec(t.Context(), `SELECT set_config('openrails.merchant_id',$1,true)`, a.String())
		require.NoError(t, err)
		_, err = f.insert(t.Context(), tx, b, customer, "forged_scope")
		var pgerr *pgconn.PgError
		require.ErrorAs(t, err, &pgerr)
		require.Equal(t, "42501", pgerr.Code)
		require.NoError(t, tx.Rollback(t.Context()))
		require.Empty(t, f.list(t, b, billing.EventQuery{}).Events)
		_, err = List(merchant.WithID(t.Context(), a), f.database, b, billing.EventQuery{}, DefaultRetention, time.Now())
		require.Equal(t, billing.CodeInvalidParam, errorCode(err))

		f.pendingEmpty(t)
	})
	t.Run("archive restore preserves log cursor and emits no new activity", func(t *testing.T) {
		mid, c := f.merchant(t)
		f.add(t, mid, c, "live")
		before := f.list(t, mid, billing.EventQuery{})
		tx, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer tx.Rollback(t.Context())
		_, err = tx.Exec(t.Context(), `SELECT set_config('test.billing_restore','true',true)`)
		require.NoError(t, err)
		_, err = f.insert(t.Context(), tx, mid, c, "restored")
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), f.sql(`INSERT INTO billing.host_outbox(id,merchant_id,event_type,subject_type,subject_id,currency) VALUES($1,$2,'delinquency.entered','customer',$3,'USD')`), uuid.New(), mid.UUID(), c)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(t.Context()))
		after := f.list(t, mid, billing.EventQuery{After: before.NextCursor})
		require.Empty(t, after.Events)
		require.Equal(t, before.NextCursor, after.NextCursor)
		f.add(t, mid, c, "after_restore")
		resumed := f.list(t, mid, billing.EventQuery{After: before.NextCursor})
		require.Len(t, resumed.Events, 1)
		require.Equal(t, "after_restore", resumed.Events[0].Type)
	})
	t.Run("host facts preserve money and typed IDs", func(t *testing.T) {
		mid, c := f.merchant(t)
		f.exec(t, `UPDATE billing.customers SET issuer='https://issuer.example.test' WHERE merchant_id=$1 AND id=$2`, mid.UUID(), c)
		payment, sub := uuid.New(), uuid.New()
		f.exec(t, `INSERT INTO billing.payments(merchant_id,id,customer_id,subscription_id) VALUES($1,$2,$3,$4)`, mid.UUID(), payment, c, sub)
		f.exec(t, `INSERT INTO billing.host_outbox(id,merchant_id,event_type,subject_type,payment_id,subject_id,amount,currency) VALUES($1,$2,'payment.settled','payment',$3,$3,9007199254740993,'USD')`, uuid.New(), mid.UUID(), payment)
		p := f.list(t, mid, billing.EventQuery{})
		require.Len(t, p.Events, 1)
		require.Equal(t, "pay_"+payment.String(), p.Events[0].Subject.ID)
		var data map[string]any
		require.NoError(t, json.Unmarshal(p.Events[0].Data, &data))
		require.Equal(t, "9007199254740993", data["amount"])
		require.Equal(t, "sub_"+sub.String(), data["subscription_id"])
		require.Equal(t, "https://issuer.example.test", data["account_issuer"])
		_, err := f.pool.Exec(t.Context(), f.sql(`UPDATE billing.event_log SET data='{}' WHERE merchant_id=$1`), mid.UUID())
		require.Error(t, err)
	})
}
