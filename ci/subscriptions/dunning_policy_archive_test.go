//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

func TestDunningPolicyArchiveRestore(t *testing.T) {
	for _, access := range []string{billing.DunningAccessKeep, billing.DunningAccessSuspend} {
		t.Run(access, func(t *testing.T) {
			source := prepareWorld(t, 12)
			source.settings.DunningPolicy = &billing.DunningPolicy{
				Tiers:                 []billing.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{24, 48}}},
				TransientRetryMinutes: []int{5, 10}, AccessDuringDunning: access, AccessWhileRenewalHeld: billing.DunningAccessSuspend,
			}
			source.start()
			e := enroll(t, source, "nmi", embedded)
			e.refreshBeforePeriodEnd()
			e.setDecline(visa.Last4, "insufficient_funds", "202")
			e.toPeriodEnd()
			first := source.clock.Now()
			source.runRenewals()
			sub := source.subscription(embedded, e.sub)
			require.Equal(t, billing.SubscriptionPastDue, sub.Status)
			require.NotNil(t, nextRetry(sub))
			require.Equal(t, 24*time.Hour, nextRetry(sub).Sub(first).Round(time.Hour))
			source.settle()
			client := source.client[embedded]

			source.stop()
			attempts := e.providerAttempts()
			providerCalls := len(source.nmi.Calls())
			mid := client.MerchantID()
			sourceDB, err := db.NewWithPGXPool(source.pool, source.schema)
			require.NoError(t, err)
			var orderID string
			require.NoError(t, source.pool.QueryRow(t.Context(), source.q(`SELECT id::text FROM billing.orders WHERE subscription_id=$1 AND last_payment_error IS NOT NULL`), e.sub.UUID()).Scan(&orderID))
			var artifact bytes.Buffer
			err = merchantarchive.Export(t.Context(), sourceDB, mid, &artifact)
			require.NoError(t, err, "archive cause: %v", errors.Unwrap(err))

			target := prepareWorldAtDSN(t, 12, providerCopyDatabase(t, dsn(t)))
			target.slug, target.auth, target.nmi, target.stripe = source.slug, source.auth, source.nmi, source.stripe
			target.clock = source.clock
			// Deployment settings differ, but cannot rewrite the restored case.
			target.settings.DunningPolicy = &billing.DunningPolicy{
				Tiers:               []billing.DunningTier{{MaxCycleHours: 96}, {RetryAfterHours: []int{36, 60}}},
				AccessDuringDunning: billing.DunningAccessKeep,
			}
			targetDB, err := db.NewWithPGXPool(target.pool, target.schema)
			require.NoError(t, err)
			directory, err := merchants.NewDirectoryService(targetDB.DataPool())
			require.NoError(t, err)
			_, _, err = directory.RegisterForRestore(t.Context(), mid, target.slug)
			require.NoError(t, err)
			tampered := rewriteProviderArchiveField(t, artifact.Bytes(), "provider_intents", "payload", func(raw string) string {
				return strings.ReplaceAll(raw, orderID, uuid.NewString())
			})
			_, err = merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(tampered))
			require.Error(t, err, "the retained outcome must stay bound to its accepted order")
			require.Zero(t, target.countRows(`SELECT count(*) FROM billing.subscriptions`), "invalid restore rolls back")
			restored, err := merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(artifact.Bytes()))
			require.NoError(t, err)
			require.False(t, restored.Replayed)
			replayed, err := merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(artifact.Bytes()))
			require.NoError(t, err)
			require.True(t, replayed.Replayed)
			var before, after string
			query := `SELECT jsonb_build_object('policy',dunning_policy,'status',status,'attempts',retry_attempts,'transient',transient_retries,'next',next_retry_at,'last',last_retry_at,'grace',grace_ends_at)::text FROM billing.subscriptions WHERE id=$1`
			require.NoError(t, source.pool.QueryRow(t.Context(), source.q(query), e.sub.UUID()).Scan(&before))
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(query), e.sub.UUID()).Scan(&after))
			require.JSONEq(t, before, after)
			query = `SELECT last_payment_error::text FROM billing.orders WHERE subscription_id=$1 AND last_payment_error IS NOT NULL`
			require.NoError(t, source.pool.QueryRow(t.Context(), source.q(query), e.sub.UUID()).Scan(&before))
			require.NoError(t, target.pool.QueryRow(t.Context(), target.q(query), e.sub.UUID()).Scan(&after))
			require.JSONEq(t, before, after, "the failed renewal keeps its explanation")
			require.Equal(t, providerCalls, len(source.nmi.Calls()), "export and restore make no provider requests")
			require.Equal(t, attempts, e.providerAttempts(), "export and restore make no charge")

			source.checkMoneyInvariants()
			source.waive("recorded", "the retired source never records the destination's recovery charge")
			armMerchant(t, target, mid)
			target.start()
			e.w, e.c.w = target, target
			require.Equal(t, access == billing.DunningAccessKeep, e.c.entitled(e.ent))
			target.advanceHealthyTo(nextRetry(sub).Add(time.Second))
			target.runRenewals()
			sub = target.subscription(embedded, e.sub)
			require.Equal(t, billing.SubscriptionPastDue, sub.Status)
			require.NotNil(t, nextRetry(sub))
			require.Equal(t, 48*time.Hour, nextRetry(sub).Sub(first).Round(time.Hour), "saved policy wins over deployment defaults")
			e.setDecline(visa.Last4, "", "")
			target.advanceHealthyTo(nextRetry(sub).Add(time.Second))
			target.runRenewals()
			require.Equal(t, billing.SubscriptionActive, target.subscription(embedded, e.sub).Status)
			require.True(t, e.c.entitled(e.ent))
			require.Equal(t, attempts+2, e.providerAttempts(), "one declined retry and one successful retry")
			target.runRenewals()
			require.Equal(t, attempts+2, e.providerAttempts(), "replay does not collect twice")
		})
	}
}
