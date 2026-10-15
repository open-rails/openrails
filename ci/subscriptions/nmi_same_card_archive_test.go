//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

func TestNMISameCardRecoveryArchiveRestore(t *testing.T) {
	source := newWorld(t)
	l := missingRecurringAgreement(t, source)
	source.nmi.DropValidationResponses(1)
	source.nmi.HideValidations(true)
	status, _ := confirmCurrentCard(l, "accepted")
	require.Equal(t, http.StatusConflict, status)
	source.nmi.HideValidations(false)
	status, body := confirmCurrentCard(l, "verified")
	require.Equal(t, http.StatusOK, status, "%v", body)
	source.runRenewals()
	require.Equal(t, billing.SubscriptionActive, source.subscription(embedded, l.sub).Status)
	require.Len(t, source.nmi.Attempts(), 1)
	source.settle()
	client := source.client[embedded]
	events, err := client.ListHostEvents(t.Context(), billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err = client.AcknowledgeHostEvents(t.Context(), []billing.HostEventID{event.ID})
		require.NoError(t, err)
	}
	source.stop()
	validations := len(source.nmi.Validations(l.railCust))
	mid := client.MerchantID()
	sourceDB, err := db.NewWithPGXPool(source.pool, source.schema)
	require.NoError(t, err)
	var artifact bytes.Buffer
	err = merchantarchive.Export(t.Context(), sourceDB, mid, &artifact)
	require.NoError(t, err, "archive cause: %v", errors.Unwrap(err))

	target := prepareWorldAtDSN(t, 12, providerCopyDatabase(t, dsn(t)))
	target.slug, target.auth, target.nmi, target.stripe = source.slug, source.auth, source.nmi, source.stripe
	target.clock = source.clock
	targetDB, err := db.NewWithPGXPool(target.pool, target.schema)
	require.NoError(t, err)
	directory, err := merchants.NewDirectoryService(targetDB.DataPool())
	require.NoError(t, err)
	_, _, err = directory.RegisterForRestore(t.Context(), mid, target.slug)
	require.NoError(t, err)
	restored, err := merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.False(t, restored.Replayed)
	replayed, err := merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	for _, table := range []string{"provider_intents", "mandates", "subscription_status_transitions", "subscription_billing_changes"} {
		var before, after string
		fields := "to_jsonb(t)"
		if table == "mandates" {
			// Attempt diagnostics are intentionally outside the archive contract;
			// the mandate's storing transaction and operation evidence remain.
			fields = "to_jsonb(t) - 'storing_attempt_id'"
		}
		query := `SELECT coalesce(jsonb_agg(` + fields + ` ORDER BY id),'[]'::jsonb)::text FROM billing.` + table + ` t WHERE subscription_id=$1`
		require.NoError(t, source.pool.QueryRow(t.Context(), source.q(query), l.sub.UUID()).Scan(&before))
		require.NoError(t, target.pool.QueryRow(t.Context(), target.q(query), l.sub.UUID()).Scan(&after))
		require.JSONEq(t, before, after, table)
	}
	require.Len(t, source.nmi.Validations(l.railCust), validations, "archive and restore make no provider request")
	armMerchant(t, target, mid)
	target.start()
	restoredLegacy := *l
	restoredLegacy.w = target
	customer := *l.c
	customer.w = target
	restoredLegacy.c = &customer
	status, body = confirmCurrentCard(&restoredLegacy, "after-restore")
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Len(t, target.nmi.Validations(l.railCust), validations)
	target.runRenewals()
	require.Equal(t, billing.SubscriptionActive, target.subscription(embedded, l.sub).Status)
	require.Len(t, target.nmi.Attempts(), 1)
	target.runRenewals()
	require.Len(t, target.nmi.Attempts(), 1)
	require.Len(t, target.nmi.Validations(l.railCust), validations)
}
