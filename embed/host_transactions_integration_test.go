//go:build integration

package embed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestHostTransactionsCommitAndRollBackWithHostRows proves the embedded
// extension on real PostgreSQL: each provider-obligation command commits or
// rolls back exactly with a host row written in the same caller-owned
// transaction, and a Client outside that transaction never sees partial work.
func TestHostTransactionsCommitAndRollBackWithHostRows(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	runtime := newProviderObligationRuntime(t, ctx, h)
	client, err := runtime.Client()
	require.NoError(t, err)
	hostTx := runtime.HostTransactions()
	pool := runtime.Embedded().App().Runtime.DB.Pool()

	_, err = h.Pool().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS public.host_provider_obligation_facts (
			operation_id text NOT NULL,
			fact text NOT NULL,
			PRIMARY KEY (operation_id, fact)
		);
		GRANT SELECT, INSERT ON public.host_provider_obligation_facts TO openrails_app`)
	require.NoError(t, err)
	hostFact := func(t *testing.T, tx pgx.Tx, operationID, fact string) {
		t.Helper()
		_, err := tx.Exec(ctx, `INSERT INTO public.host_provider_obligation_facts (operation_id, fact) VALUES ($1, $2)`, operationID, fact)
		require.NoError(t, err)
	}
	hostFactExists := func(t *testing.T, operationID, fact string) bool {
		t.Helper()
		var exists bool
		require.NoError(t, h.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.host_provider_obligation_facts WHERE operation_id = $1 AND fact = $2)`, operationID, fact).Scan(&exists))
		return exists
	}
	inHostTx := func(t *testing.T, commit bool, fn func(tx pgx.Tx)) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()
		fn(tx)
		if commit {
			require.NoError(t, tx.Commit(ctx))
		} else {
			require.NoError(t, tx.Rollback(ctx))
		}
	}

	f := newProviderFixture(t, ctx, h, client, 10_000)
	a := f.authorization("host-open", 4_000)

	t.Run("open rolls back with the host obligation", func(t *testing.T) {
		inHostTx(t, false, func(tx pgx.Tx) {
			hostFact(t, tx, a.OperationID, "provider_obligation")
			opened, err := hostTx.OpenOperationAuthorization(ctx, tx, a)
			require.NoError(t, err)
			require.Equal(t, openrails.OperationAuthorizationOpen, opened.State)
			read, err := hostTx.GetOperationAuthorization(ctx, tx, a.OperationID)
			require.NoError(t, err)
			require.Equal(t, opened, read, "a Tx read observes its own uncommitted open")
			_, err = client.GetOperationAuthorization(ctx, a.OperationID)
			require.ErrorIs(t, err, openrails.ErrOperationAuthorizationNotFound, "no other transaction sees the reservation before commit")
		})
		_, err := client.GetOperationAuthorization(ctx, a.OperationID)
		require.ErrorIs(t, err, openrails.ErrOperationAuthorizationNotFound)
		require.False(t, hostFactExists(t, a.OperationID, "provider_obligation"))
		require.Equal(t, "balance=10000 held=0 available=10000 owed=0", f.account(t, ctx))
	})

	t.Run("open commits with the host obligation", func(t *testing.T) {
		inHostTx(t, true, func(tx pgx.Tx) {
			hostFact(t, tx, a.OperationID, "provider_obligation")
			_, err := hostTx.OpenOperationAuthorization(ctx, tx, a)
			require.NoError(t, err)
		})
		read, err := client.GetOperationAuthorization(ctx, a.OperationID)
		require.NoError(t, err)
		require.Equal(t, openrails.OperationAuthorizationOpen, read.State)
		require.True(t, hostFactExists(t, a.OperationID, "provider_obligation"))
		require.Equal(t, "balance=10000 held=4000 available=6000 owed=0", f.account(t, ctx))
	})

	release := openrails.ReleaseOperationAuthorizationRequest{OperationID: a.OperationID, ReleaseReference: "provider-absent:" + uuid.NewString()}
	t.Run("release rolls back with the host absence fact", func(t *testing.T) {
		inHostTx(t, false, func(tx pgx.Tx) {
			hostFact(t, tx, a.OperationID, "provider_absent")
			released, err := hostTx.ReleaseOperationAuthorization(ctx, tx, release)
			require.NoError(t, err)
			require.Equal(t, openrails.OperationAuthorizationReleased, released.State)
			read, err := hostTx.GetOperationAuthorization(ctx, tx, a.OperationID)
			require.NoError(t, err)
			require.Equal(t, openrails.OperationAuthorizationReleased, read.State)
			outside, err := client.GetOperationAuthorization(ctx, a.OperationID)
			require.NoError(t, err)
			require.Equal(t, openrails.OperationAuthorizationOpen, outside.State)
		})
		read, err := client.GetOperationAuthorization(ctx, a.OperationID)
		require.NoError(t, err)
		require.Equal(t, openrails.OperationAuthorizationOpen, read.State)
		require.False(t, hostFactExists(t, a.OperationID, "provider_absent"))
		require.Equal(t, "balance=10000 held=4000 available=6000 owed=0", f.account(t, ctx))
	})

	t.Run("release commits with the host absence fact", func(t *testing.T) {
		inHostTx(t, true, func(tx pgx.Tx) {
			hostFact(t, tx, a.OperationID, "provider_absent")
			_, err := hostTx.ReleaseOperationAuthorization(ctx, tx, release)
			require.NoError(t, err)
		})
		read, err := client.GetOperationAuthorization(ctx, a.OperationID)
		require.NoError(t, err)
		require.Equal(t, openrails.OperationAuthorizationReleased, read.State)
		require.Equal(t, release.ReleaseReference, read.TerminalReference)
		require.True(t, hostFactExists(t, a.OperationID, "provider_absent"))
		require.Equal(t, "balance=10000 held=0 available=10000 owed=0", f.account(t, ctx))
	})

	b := f.authorization("host-settle", 1_000)
	inHostTx(t, true, func(tx pgx.Tx) {
		_, err := hostTx.OpenOperationAuthorization(ctx, tx, b)
		require.NoError(t, err)
		baseline, err := hostTx.RecordProviderBillingObservation(ctx, tx, f.observation(b.OperationID, "baseline", 2_500))
		require.NoError(t, err)
		require.Equal(t, openrails.ProviderBillingQualificationPending, baseline.State)
	})
	time.Sleep(providerBillingTestQuiescence + 100*time.Millisecond)
	qualifying := f.observation(b.OperationID, "qualifying", 2_500)

	t.Run("settlement rolls back with the host billing fact", func(t *testing.T) {
		inHostTx(t, false, func(tx pgx.Tx) {
			hostFact(t, tx, b.OperationID, "billing_closed")
			settled, err := hostTx.RecordProviderBillingObservation(ctx, tx, qualifying)
			require.NoError(t, err)
			require.Equal(t, openrails.ProviderBillingQualificationEligible, settled.State)
			require.Equal(t, openrails.OperationAuthorizationSettled, settled.Authorization.State)
			read, err := hostTx.GetProviderBillingQualification(ctx, tx, b.OperationID)
			require.NoError(t, err)
			require.Equal(t, openrails.OperationAuthorizationSettled, read.Authorization.State)
		})
		read, err := client.GetProviderBillingQualification(ctx, b.OperationID)
		require.NoError(t, err)
		require.Equal(t, openrails.ProviderBillingQualificationPending, read.State)
		require.Equal(t, openrails.OperationAuthorizationOpen, read.Authorization.State)
		require.False(t, hostFactExists(t, b.OperationID, "billing_closed"))
		require.Equal(t, "balance=10000 held=1000 available=9000 owed=0", f.account(t, ctx), "a rolled-back settlement moves no money")
	})

	t.Run("settlement commits with the host billing fact", func(t *testing.T) {
		inHostTx(t, true, func(tx pgx.Tx) {
			hostFact(t, tx, b.OperationID, "billing_closed")
			_, err := hostTx.RecordProviderBillingObservation(ctx, tx, qualifying)
			require.NoError(t, err)
		})
		read, err := client.GetProviderBillingQualification(ctx, b.OperationID)
		require.NoError(t, err)
		require.Equal(t, openrails.ProviderBillingQualificationEligible, read.State)
		require.Equal(t, openrails.OperationAuthorizationSettled, read.Authorization.State)
		require.EqualValues(t, 2_500, *read.Authorization.SettlementRatedUSDMicros)
		require.True(t, hostFactExists(t, b.OperationID, "billing_closed"))
		require.Equal(t, "balance=7500 held=0 available=7500 owed=0", f.account(t, ctx))

		replay, err := client.RecordProviderBillingObservation(ctx, qualifying)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Equal(t, "balance=7500 held=0 available=7500 owed=0", f.account(t, ctx), "replay moves money once")
	})

	t.Run("errors classify like the Client", func(t *testing.T) {
		changed := a
		changed.ClaimReference = "claim:changed"
		attempts := []struct {
			name     string
			host     func(pgx.Tx) error
			remote   func() error
			sentinel error
			class    error
		}{
			{
				"missing",
				func(tx pgx.Tx) error { _, err := hostTx.GetOperationAuthorization(ctx, tx, "missing"); return err },
				func() error { _, err := client.GetOperationAuthorization(ctx, "missing"); return err },
				openrails.ErrOperationAuthorizationNotFound, openrails.ErrNotFound,
			},
			{
				"release with evidence",
				func(tx pgx.Tx) error {
					_, err := hostTx.ReleaseOperationAuthorization(ctx, tx, openrails.ReleaseOperationAuthorizationRequest{OperationID: b.OperationID, ReleaseReference: "absent"})
					return err
				},
				func() error {
					_, err := client.ReleaseOperationAuthorization(ctx, openrails.ReleaseOperationAuthorizationRequest{OperationID: b.OperationID, ReleaseReference: "absent"})
					return err
				},
				openrails.ErrOperationAuthorizationHasBillingEvidence, openrails.ErrConflict,
			},
			{
				"insufficient",
				func(tx pgx.Tx) error {
					_, err := hostTx.OpenOperationAuthorization(ctx, tx, f.authorization("over", 1_000_000))
					return err
				},
				func() error {
					_, err := client.OpenOperationAuthorization(ctx, f.authorization("over", 1_000_000))
					return err
				},
				openrails.ErrInsufficientCredits, openrails.ErrInsufficientCredits,
			},
			{
				"invalid",
				func(tx pgx.Tx) error {
					_, err := hostTx.OpenOperationAuthorization(ctx, tx, f.authorization("zero", 0))
					return err
				},
				func() error { _, err := client.OpenOperationAuthorization(ctx, f.authorization("zero", 0)); return err },
				openrails.ErrInvalid, openrails.ErrInvalid,
			},
			{
				"conflict",
				func(tx pgx.Tx) error { _, err := hostTx.OpenOperationAuthorization(ctx, tx, changed); return err },
				func() error { _, err := client.OpenOperationAuthorization(ctx, changed); return err },
				openrails.ErrOperationAuthorizationConflict, openrails.ErrConflict,
			},
		}
		for _, attempt := range attempts {
			var hostErr error
			// Each host attempt ends before the Client call so neither waits on
			// the other's payer lock.
			inHostTx(t, false, func(tx pgx.Tx) { hostErr = attempt.host(tx) })
			remoteErr := attempt.remote()
			for label, err := range map[string]error{"host": hostErr, "client": remoteErr} {
				require.ErrorIs(t, err, attempt.sentinel, "%s %s", label, attempt.name)
				require.ErrorIs(t, err, attempt.class, "%s %s", label, attempt.name)
			}
			if attempt.name == "conflict" {
				var typed *openrails.OperationAuthorizationConflict
				var status *openrails.StatusError
				require.True(t, errors.As(hostErr, &typed))
				require.True(t, errors.As(remoteErr, &status))
				require.NotNil(t, status.Param)
				require.Equal(t, typed.Field, *status.Param)
			}
		}
	})

	t.Run("a transaction bound to another merchant is refused", func(t *testing.T) {
		inHostTx(t, false, func(tx pgx.Tx) {
			_, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, db.MerchantGUC, uuid.NewString())
			require.NoError(t, err)
			_, err = hostTx.OpenOperationAuthorization(ctx, tx, f.authorization("foreign", 1))
			var unscoped *db.ErrUnscopedMerchantWork
			require.True(t, errors.As(err, &unscoped), "got %v", err)
		})
		_, err := hostTx.GetOperationAuthorization(merchant.WithID(ctx, merchant.ID(uuid.New())), nil, a.OperationID)
		require.ErrorIs(t, err, openrails.ErrConflict)
	})
}
