package db

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// The connect wait has no budget of its own: a late database is connected to,
// and only the caller's context ends the wait.
func TestPingWithRetryWaitsUntilTheCallerStops(t *testing.T) {
	var calls atomic.Int32
	start := time.Now()
	require.NoError(t, pingWithRetry(context.Background(), func(context.Context) error {
		if calls.Add(1) < 2 {
			return errors.New("connection refused")
		}
		return nil
	}, "db"))
	require.EqualValues(t, 2, calls.Load())
	require.GreaterOrEqual(t, time.Since(start), dbConnectBaseDelay, "backs off between attempts")

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := pingWithRetry(ctx, func(context.Context) error { return errors.New("still failing over") }, "db")
	require.ErrorIs(t, err, context.Canceled)
	require.Contains(t, err.Error(), "still failing over", "the last observed failure travels with the reason")

	done, stop := context.WithCancel(context.Background())
	stop()
	require.ErrorIs(t, pingWithRetry(done, func(context.Context) error { return errors.New("not ready") }, "db"), context.Canceled)
}

type recordingTx struct {
	pgx.Tx
	sql []string
}

func (r *recordingTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	r.sql = append(r.sql, sql)
	return pgconn.CommandTag{}, nil
}

func TestSchemaRewriteRelocatesOnlyTheOpenRailsQualifier(t *testing.T) {
	const in = "INSERT INTO openrails.payments (id) SELECT public.gen_random_uuid() FROM profiles.users p JOIN openrails.customers c ON c.id = p.id"
	for schema, want := range map[string]string{
		config.CanonicalSchema: in,
		"":                     strings.ReplaceAll(in, "openrails.", "billing."),
		config.DefaultSchema:   strings.ReplaceAll(in, "openrails.", "billing."),
		"shop":                 strings.ReplaceAll(in, "openrails.", "shop."),
	} {
		rw := newSchemaRewriter(schema)
		require.Equal(t, want, rw.apply(in), schema)
		wantSchema := schema
		if schema == "" {
			wantSchema = config.DefaultSchema
		}
		require.Equal(t, wantSchema, rw.schema())
		require.Equal(t, wantSchema, (&DB{rw: rw, pool: new(pgxpool.Pool)}).DataPool().Schema(), "DataPool must report the true schema")

		// Every execution path carries the rewrite, including transactions.
		tx := &recordingTx{}
		_, err := rw.wrapTx(tx).Exec(context.Background(), in)
		require.NoError(t, err)
		_, err = RewriteDBTX(tx, schema).Exec(context.Background(), in)
		require.NoError(t, err)
		require.Equal(t, []string{want, want}, tx.sql, schema)
	}

	// A raw host transaction has no schema of its own and gets the default;
	// one begun by a configured DB keeps that DB's schema.
	raw := &recordingTx{}
	_, err := NewWithPgxTx(raw).Qx(context.Background()).Exec(context.Background(), "SELECT 1 FROM openrails.x")
	require.NoError(t, err)
	custom := &recordingTx{}
	_, err = (&DB{rw: newSchemaRewriter("shop")}).NewWithPgxTx(custom).Qx(context.Background()).Exec(context.Background(), "SELECT 1 FROM openrails.x")
	require.NoError(t, err)
	canonical := &recordingTx{}
	_, err = NewWithPgxTx(newSchemaRewriter(config.CanonicalSchema).wrapTx(canonical)).Qx(context.Background()).Exec(context.Background(), "SELECT 1 FROM openrails.x")
	require.NoError(t, err)
	require.Equal(t, []string{"SELECT 1 FROM billing.x"}, raw.sql)
	require.Equal(t, []string{"SELECT 1 FROM shop.x"}, custom.sql)
	require.Equal(t, []string{"SELECT 1 FROM openrails.x"}, canonical.sql)
}

// Provider-bound rows must carry the PSP that produced them; nothing invents one.
func TestProvenancePinsRefuseTheNilUUID(t *testing.T) {
	ctx := context.Background()
	_, err := RequirePSPID(ctx)
	require.ErrorIs(t, err, ErrNoPSPInContext)
	_, err = RequirePSPID(WithPSPID(ctx, uuid.Nil))
	require.ErrorIs(t, err, ErrNoPSPInContext)
	id := uuid.New()
	got, err := RequirePSPID(WithPSPID(WithPSPID(ctx, id), uuid.Nil))
	require.NoError(t, err)
	require.Equal(t, id, got, "a nil pin must not clear an existing one")

	_, err = RequireCustodianID(WithCustodianID(ctx, uuid.Nil))
	require.Error(t, err)
	got, err = RequireCustodianID(WithCustodianID(ctx, id))
	require.NoError(t, err)
	require.Equal(t, id, got)
}

func TestCustomerIdentityIsUUIDOnly(t *testing.T) {
	uid := uuid.New()
	for in, want := range map[string]uuid.UUID{uid.String(): uid, " " + strings.ToUpper(uid.String()) + " ": uid, "": uuid.Nil, "  ": uuid.Nil} {
		got, err := ResolveCustomerID(in)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := ResolveCustomerID("legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")
	got, err := EnsureCustomerID(context.Background(), nil, uuid.Nil, "")
	require.NoError(t, err, "empty subject must not touch the database")
	require.Equal(t, uuid.Nil, got)
	_, err = EnsureCustomerID(context.Background(), nil, uuid.Nil, "legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")
	_, err = EnsureCustomerID(context.Background(), nil, uuid.Nil, uid.String())
	require.Error(t, err, "a subject without an explicit or contextual merchant is refused")

	a, b := uuid.MustParse("10000000-0000-4000-8000-000000000001"), uuid.MustParse("20000000-0000-4000-8000-000000000002")
	require.Equal(t, SystemCustomerID(a), SystemCustomerID(a))
	require.NotEqual(t, SystemCustomerID(a), SystemCustomerID(b))
	require.Equal(t, uuid.Version(5), SystemCustomerID(a).Version())
}
