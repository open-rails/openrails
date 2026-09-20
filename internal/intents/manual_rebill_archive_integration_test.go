//go:build integration

package intents_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestManualRebillArchivePreservesAcceptedTermsAndTerminalCustody(t *testing.T) {
	for _, outcome := range []string{"paid", "refused"} {
		t.Run(outcome, func(t *testing.T) {
			source, mid, original, calls := intents.TerminalRebillArchiveFixture(t, outcome == "refused")
			var archive bytes.Buffer
			sourceCtx, sourceRelease, err := source.WithMerchantConn(merchant.WithID(t.Context(), mid))
			require.NoError(t, err)
			events, err := source.Gen(sourceCtx).ListHostEvents(sourceCtx, gen.ListHostEventsParams{MerchantID: mid.UUID(), RowLimit: 100})
			require.NoError(t, err)
			if outcome == "paid" {
				require.Len(t, events, 1)
				require.Error(t, merchantarchive.Export(t.Context(), source, mid, &archive), "a pending host reaction is not silently discarded")
				require.Empty(t, archive.Bytes())
			}
			for _, event := range events {
				n, err := source.Gen(sourceCtx).AcknowledgeHostEvent(sourceCtx, gen.AcknowledgeHostEventParams{MerchantID: mid.UUID(), ID: event.ID, Now: time.Now().UTC()})
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
			}
			sourceRelease()
			require.NoError(t, merchantarchive.Export(t.Context(), source, mid, &archive))
			adminDSN, appDSN := dbtest.SharedRLSPostgres(t)
			schema := "rebill_archive_" + outcome
			require.NoError(t, migrate.RunPostgres(t.Context(), &config.Config{DB: &config.DBConfig{URL: adminDSN, Schema: schema}}))
			target, err := db.NewDB(t.Context(), &config.DBConfig{URL: appDSN, Schema: schema})
			require.NoError(t, err)
			t.Cleanup(func() { _ = target.Close() })
			_, err = target.Qx(t.Context()).Exec(t.Context(), `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, mid.UUID(), schema)
			require.NoError(t, err)
			_, err = merchantarchive.Restore(t.Context(), target, mid, bytes.NewReader(archive.Bytes()))
			require.NoError(t, err, "%v", errors.Unwrap(err))
			ctx, release, err := target.WithMerchantConn(merchant.WithID(context.Background(), mid))
			require.NoError(t, err)
			defer release()
			// No provider adapter is available in the destination. Terminal
			// replay must return the immutable result without a new side effect.
			restored, err := (&intents.Runner{Store: intents.NewStore(target)}).ExecuteByID(ctx, original.ID)
			require.NoError(t, err)
			require.Equal(t, original.Status, restored.Status)
			require.JSONEq(t, string(original.Payload), string(restored.Payload))
			require.JSONEq(t, string(original.ResultEvidence), string(restored.ResultEvidence))
			require.NoError(t, intents.ValidateManualRebillTerminal(restored))
			require.EqualValues(t, 1, calls())
		})
	}
}
