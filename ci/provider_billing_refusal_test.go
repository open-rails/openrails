//go:build e2e && integration

package ci_test

import (
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchant"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

// A host refuses a cost it cannot qualify with an evidence-free observation.
// The refused hold then takes no evidence, growth or release; the list shows
// every stuck hold with its reason; an operator's close, settled or written
// off, is its only exit.
func TestProviderBillingRefusal(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "refuse-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	table := func(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }

	fund := func(amount int64) billing.CustomerID {
		customer := billing.CustomerID(uuid.New())
		_, err := createCreditGrant(ctx, client, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: amount, Source: "support", SourceID: uuid.NewString()})
		require.NoError(t, err)
		return customer
	}
	open := func(customer billing.CustomerID, operationID string, amount int64) {
		t.Helper()
		body := []byte(`{"rental":"` + operationID + `"}`)
		_, err := client.OpenProviderOperation(ctx, billing.OpenProviderOperationParams{
			OperationID: operationID, CustomerID: customer, RecordOwner: "user:1", Currency: "USD", Amount: amount,
			ClaimReference: "claim:" + operationID, AuthorizationBody: body, AuthorizationBodySHA256: billing.SHA256(sha256.Sum256(body)),
		})
		require.NoError(t, err)
		time.Sleep(time.Millisecond)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	start, end := now.Add(-2*time.Hour), now.Add(-time.Hour)
	observation := func(operationID, observationID string, costs ...int64) billing.RecordProviderBillingObservationParams {
		in := billing.RecordProviderBillingObservationParams{
			OperationID: operationID, ObservationID: observationID,
			Lifecycle: billing.ProviderBillingLifecycleEvidence{
				Provider: "provider", ProviderResourceID: "pod-" + operationID,
				ProviderLifetimeStartsAt: start, ProviderLifetimeEndsAt: end, ProviderAbsentAt: end,
				ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
				WindowsClosedAt: end, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{"absent":true}`),
			},
			NormalizedQuery: "pod=" + operationID, QueryStartsAt: start, QueryEndsAt: end, RawBody: []byte(`[{"amount":1}]`),
		}
		for _, cost := range costs {
			in.Records = append(in.Records, billing.ProviderBillingRecord{ProviderResourceID: "pod-" + operationID, BucketStart: start, Amount: cost, TimeBilledMS: 60_000})
		}
		return in
	}
	refuse := func(operationID, observationID string, kind billing.ProviderBillingRefusalKind, detail string) billing.RecordProviderBillingObservationParams {
		return billing.RecordProviderBillingObservationParams{OperationID: operationID, ObservationID: observationID,
			Refusal: &billing.ProviderBillingObservationRefusal{Kind: kind, Detail: detail}}
	}
	balance := func(customer billing.CustomerID) billing.Balance {
		bal, err := client.GetBalance(ctx, customer, "USD")
		require.NoError(t, err)
		return *bal
	}
	refused := func(err error, status int, code, param string) {
		t.Helper()
		var se *billing.StatusError
		require.ErrorAs(t, err, &se)
		require.Equal(t, status, se.Status, se.Error())
		require.Equal(t, code, se.Code)
		if param != "" {
			require.NotNil(t, se.Param)
			require.Equal(t, param, *se.Param)
		}
	}
	cost := func(v int64) *int64 { return &v }
	yes, no := true, false
	list := func(params billing.ProviderOperationListParams) map[string]string {
		t.Helper()
		out := map[string]string{}
		var order []string
		for {
			page, err := client.ListProviderOperations(ctx, params)
			require.NoError(t, err)
			for _, auth := range page.Items {
				order = append(order, auth.OperationID)
				out[auth.OperationID] = ""
				if auth.Refusal != nil {
					out[auth.OperationID] = string(auth.Refusal.Reason)
				}
			}
			if page.Next == "" {
				out["order"] = strings.Join(order, ",")
				return out
			}
			params.Cursor = page.Next
		}
	}
	stuck := billing.ProviderOperationListParams{State: []billing.ProviderOperationState{billing.ProviderOperationOpen}, Refused: &yes}

	debtor, payer, evidence, other := fund(400_000), fund(1_000_000), fund(1_000_000), fund(1_000_000)
	open(other, "released", 50_000)
	_, err := client.ReleaseProviderOperation(ctx, billing.ReleaseProviderOperationParams{OperationID: "released", ReleaseReference: "never-created"})
	require.NoError(t, err)
	open(other, "live", 100_000)
	open(debtor, "unprovable", 300_000)
	open(payer, "unreadable", 200_000)
	open(evidence, "rejected", 100_000)
	baseline, err := client.RecordProviderBillingObservation(ctx, observation("rejected", "rejected:1", 30_000))
	require.NoError(t, err)
	require.Equal(t, billing.ProviderBillingAwaitingEqualObservation, baseline.Qualification.Reason)

	t.Run("the host refuses a hold it cannot qualify", func(t *testing.T) {
		req := refuse("unprovable", "unprovable:host", billing.ProviderBillingRefusalLifecycleUnprovable, "window 7 of an absent resource is open")
		op, err := client.RecordProviderBillingObservation(ctx, req)
		require.NoError(t, err)
		require.False(t, op.Replayed)
		require.Equal(t, billing.ProviderOperationOpen, op.State)
		require.NotNil(t, op.Refusal)
		require.Equal(t, billing.ProviderBillingLifecycleUnprovable, op.Refusal.Reason)
		require.Equal(t, "observation unprovable:host: window 7 of an absent resource is open", op.Refusal.Detail)
		require.Nil(t, op.Qualification, "a host refusal carries no evidence")
		require.Nil(t, op.Resolution)

		replay, err := client.RecordProviderBillingObservation(ctx, req)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Equal(t, op.Refusal, replay.Refusal)
		changed := req
		changed.Refusal = &billing.ProviderBillingObservationRefusal{Kind: req.Refusal.Kind, Detail: "another reason"}
		_, err = client.RecordProviderBillingObservation(ctx, changed)
		refused(err, 409, "provider_billing_observation_conflict", "refusal_detail")
		changed.Refusal = &billing.ProviderBillingObservationRefusal{Kind: billing.ProviderBillingRefusalUnavailable, Detail: req.Refusal.Detail}
		_, err = client.RecordProviderBillingObservation(ctx, changed)
		refused(err, 409, "provider_billing_observation_conflict", "refusal_kind")
		_, err = client.RecordProviderBillingObservation(ctx, refuse("unprovable", "unprovable:again", billing.ProviderBillingRefusalUnavailable, ""))
		refused(err, 409, "provider_operation_refused", "")

		withEvidence := observation("live", "live:host", 10)
		withEvidence.Records, withEvidence.RawBody = nil, nil
		withEvidence.Refusal = &billing.ProviderBillingObservationRefusal{Kind: billing.ProviderBillingRefusalUnavailable}
		for _, bad := range []billing.RecordProviderBillingObservationParams{
			refuse("live", "live:host", "stuck", ""),
			refuse("live", "", billing.ProviderBillingRefusalUnavailable, ""),
			refuse("live", "live:host", billing.ProviderBillingRefusalUnavailable, "trailing "),
			withEvidence,
		} {
			_, err := client.RecordProviderBillingObservation(ctx, bad)
			refused(err, 400, "invalid_param", "")
		}
		_, err = client.RecordProviderBillingObservation(ctx, refuse("missing", "missing:host", billing.ProviderBillingRefusalUnavailable, ""))
		refused(err, 404, "provider_operation_not_found", "")
		_, err = client.RecordProviderBillingObservation(ctx, refuse("released", "released:host", billing.ProviderBillingRefusalUnavailable, ""))
		refused(err, 409, "provider_operation_not_open", "")
		_, err = client.RecordProviderBillingObservation(ctx, refuse("rejected", "rejected:1", billing.ProviderBillingRefusalObservationRejected, ""))
		refused(err, 409, "provider_billing_observation_conflict", "refusal_kind")

		got, err := client.GetProviderOperation(ctx, "live")
		require.NoError(t, err)
		require.Nil(t, got.Refusal, "a refused refusal writes nothing")

		_, err = client.RecordProviderBillingObservation(ctx, refuse("unreadable", "unreadable:host", billing.ProviderBillingRefusalUnavailable, ""))
		require.NoError(t, err)
		rejected, err := client.RecordProviderBillingObservation(ctx, refuse("rejected", "rejected:host", billing.ProviderBillingRefusalObservationRejected,
			"provider_billing_observation_conflict: lifecycle_evidence_body"))
		require.NoError(t, err)
		require.Equal(t, billing.ProviderBillingObservationRejected, rejected.Refusal.Reason)
		require.Equal(t, billing.ProviderBillingQualificationPending, rejected.Qualification.State, "a host refusal leaves the qualification as it was")
	})

	t.Run("a refused hold takes no evidence, growth or release", func(t *testing.T) {
		_, err := client.RecordProviderBillingObservation(ctx, observation("unprovable", "unprovable:1", 10))
		refused(err, 409, "provider_operation_refused", "")
		unprovable, err := client.GetProviderOperation(ctx, "unprovable")
		require.NoError(t, err)
		require.Nil(t, unprovable.Qualification)
		_, err = client.RecordProviderBillingObservation(ctx, observation("rejected", "rejected:2", 30_000))
		refused(err, 409, "provider_operation_refused", "")
		replay, err := client.RecordProviderBillingObservation(ctx, observation("rejected", "rejected:1", 30_000))
		require.NoError(t, err, "an observation recorded before the refusal still replays")
		require.True(t, replay.Replayed)
		require.Equal(t, billing.ProviderBillingObservationRejected, replay.Refusal.Reason)

		_, err = client.IncrementProviderOperation(ctx, billing.IncrementProviderOperationParams{OperationID: "unprovable", Ordinal: 1, Amount: 10, MinimumAmount: 10})
		refused(err, 409, "provider_operation_refused", "")
		_, err = client.ReleaseProviderOperation(ctx, billing.ReleaseProviderOperationParams{OperationID: "unprovable", ReleaseReference: "never-created"})
		refused(err, 409, "provider_operation_refused", "")
		_, err = client.ReleaseProviderOperation(ctx, billing.ReleaseProviderOperationParams{OperationID: "rejected", ReleaseReference: "never-created"})
		refused(err, 409, "provider_operation_has_billing_evidence", "")
		require.EqualValues(t, 300_000, balance(debtor).HeldAmount)
	})

	t.Run("the list shows every stuck hold with its reason", func(t *testing.T) {
		want := map[string]string{
			"rejected": "observation_rejected", "unreadable": "provider_billing_unavailable", "unprovable": "lifecycle_unprovable",
			"order": "rejected,unreadable,unprovable",
		}
		require.Equal(t, want, list(stuck))
		paged := stuck
		paged.Limit = 1
		require.Equal(t, want, list(paged))
		require.Equal(t, map[string]string{"live": "", "order": "live"},
			list(billing.ProviderOperationListParams{State: []billing.ProviderOperationState{billing.ProviderOperationOpen}, Refused: &no}))
		require.Equal(t, "rejected,unreadable,unprovable,live,released", list(billing.ProviderOperationListParams{})["order"])
		require.Equal(t, "rejected,unreadable,unprovable", list(billing.ProviderOperationListParams{Refused: &yes})["order"])
		require.Equal(t, "released", list(billing.ProviderOperationListParams{State: []billing.ProviderOperationState{billing.ProviderOperationReleased}})["order"])

		_, err := client.ListProviderOperations(ctx, billing.ProviderOperationListParams{State: []billing.ProviderOperationState{"stuck"}})
		refused(err, 400, "invalid_query", "state")
		_, err = client.ListProviderOperations(ctx, billing.ProviderOperationListParams{PageRequest: billing.PageRequest{Cursor: "not-a-cursor"}})
		refused(err, 400, "invalid_cursor", "cursor")
	})

	t.Run("close needs a refused hold", func(t *testing.T) {
		settle := billing.CloseProviderOperationParams{OperationID: "unprovable", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(1), AttestedBy: "operator:paul", Reference: "invoice:1"}
		for _, bad := range []func(*billing.CloseProviderOperationParams){
			func(r *billing.CloseProviderOperationParams) { r.CostAmount = nil },
			func(r *billing.CloseProviderOperationParams) { r.CostAmount = cost(-1) },
			func(r *billing.CloseProviderOperationParams) {
				r.Kind = billing.ProviderBillingResolutionWrittenOff
			},
			func(r *billing.CloseProviderOperationParams) { r.Kind = "refunded" },
			func(r *billing.CloseProviderOperationParams) { r.AttestedBy = "" },
			func(r *billing.CloseProviderOperationParams) { r.Reference = " invoice:1" },
		} {
			req := settle
			bad(&req)
			_, err := client.CloseProviderOperation(ctx, req)
			refused(err, 400, "invalid_param", "")
		}
		req := settle
		req.OperationID = "live"
		_, err := client.CloseProviderOperation(ctx, req)
		refused(err, 409, "provider_operation_not_refused", "")
		req.OperationID = "missing"
		_, err = client.CloseProviderOperation(ctx, req)
		refused(err, 404, "provider_operation_not_found", "")
		require.EqualValues(t, 300_000, balance(debtor).HeldAmount, "a refused close writes nothing")
	})

	t.Run("settled closes a hold that has no evidence, above the hold as owed", func(t *testing.T) {
		req := billing.CloseProviderOperationParams{
			OperationID: "unprovable", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(450_000),
			AttestedBy: "operator:paul", Reference: "runpod-invoice:2026-10", Note: "lifecycle unprovable; invoiced cost",
		}
		auth, err := client.CloseProviderOperation(ctx, req)
		require.NoError(t, err)
		require.False(t, auth.Replayed)
		require.Equal(t, billing.ProviderOperationSettled, auth.State)
		require.EqualValues(t, 450_000, *auth.SettlementCostAmount)
		require.EqualValues(t, 450_000, *auth.SettlementAmount)
		require.Equal(t, "sha256:"+auth.SettlementBodySHA256.String(), auth.TerminalReference)
		require.Equal(t, billing.ProviderBillingLifecycleUnprovable, auth.Refusal.Reason)
		require.Equal(t, billing.ProviderBillingResolutionSettled, auth.Resolution.Kind)
		require.EqualValues(t, 450_000, *auth.Resolution.CostAmount)
		require.Equal(t, req.Note, auth.Resolution.Note)

		var manifest struct {
			Contract            string          `json:"contract"`
			OperationID         string          `json:"operation_id"`
			Provider            *string         `json:"provider"`
			QualificationReason *string         `json:"qualification_reason"`
			RefusedObservation  json.RawMessage `json:"refused_observation"`
			Refusal             struct {
				Reason string `json:"reason"`
				Detail string `json:"detail"`
			} `json:"refusal"`
			Authorization struct {
				AuthorizedAmount string `json:"authorized_amount"`
			} `json:"authorization"`
			CostAmount string `json:"cost_amount"`
			AttestedBy string `json:"attested_by"`
			Reference  string `json:"reference"`
		}
		require.NoError(t, json.Unmarshal(auth.SettlementBody, &manifest))
		require.Equal(t, "openrails/operator-attested-provider-cost", manifest.Contract)
		require.Equal(t, "unprovable", manifest.OperationID)
		require.Nil(t, manifest.Provider, "a hold without a qualification has no lifecycle")
		require.Nil(t, manifest.QualificationReason)
		require.Equal(t, "null", string(manifest.RefusedObservation))
		require.Equal(t, "lifecycle_unprovable", manifest.Refusal.Reason)
		require.Equal(t, "observation unprovable:host: window 7 of an absent resource is open", manifest.Refusal.Detail)
		require.Equal(t, "300000", manifest.Authorization.AuthorizedAmount)
		require.Equal(t, "450000", manifest.CostAmount)
		require.Equal(t, "operator:paul", manifest.AttestedBy)
		require.Equal(t, "runpod-invoice:2026-10", manifest.Reference)

		bal := balance(debtor)
		require.EqualValues(t, 0, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.HeldAmount)
		require.EqualValues(t, 50_000, bal.OwedAmount)

		replay, err := client.CloseProviderOperation(ctx, req)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Equal(t, auth.TerminalReference, replay.TerminalReference)
		require.Equal(t, auth.Resolution, replay.Resolution)
		require.Equal(t, bal, balance(debtor), "a replay charges nothing")
		changed := req
		changed.CostAmount = cost(400_000)
		_, err = client.CloseProviderOperation(ctx, changed)
		refused(err, 409, "provider_operation_conflict", "cost_amount")
		changed = req
		changed.AttestedBy = "operator:other"
		_, err = client.CloseProviderOperation(ctx, changed)
		refused(err, 409, "provider_operation_conflict", "attested_by")

		got, err := client.GetProviderOperation(ctx, "unprovable")
		require.NoError(t, err)
		require.Equal(t, auth.Refusal, got.Refusal)
		require.Equal(t, auth.Resolution, got.Resolution)
		_, err = client.RecordProviderBillingObservation(ctx, observation("unprovable", "unprovable:2", 10))
		refused(err, 409, "provider_operation_not_open", "")
	})

	t.Run("written_off releases a refused hold uncharged", func(t *testing.T) {
		req := billing.CloseProviderOperationParams{
			OperationID: "unreadable", Kind: billing.ProviderBillingResolutionWrittenOff, AttestedBy: "operator:paul", Reference: "th-045:vast-has-no-billing-reader",
		}
		require.EqualValues(t, 200_000, balance(payer).HeldAmount)
		auth, err := client.CloseProviderOperation(ctx, req)
		require.NoError(t, err)
		require.Equal(t, billing.ProviderOperationReleased, auth.State)
		require.Equal(t, req.Reference, auth.TerminalReference)
		require.Nil(t, auth.SettlementAmount)
		require.Equal(t, billing.ProviderBillingResolutionWrittenOff, auth.Resolution.Kind)
		require.Nil(t, auth.Resolution.CostAmount)
		bal := balance(payer)
		require.EqualValues(t, 1_000_000, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.HeldAmount)
		require.EqualValues(t, 0, bal.OwedAmount)
		_, err = client.ReleaseProviderOperation(ctx, billing.ReleaseProviderOperationParams{OperationID: "unreadable", ReleaseReference: req.Reference})
		refused(err, 409, "provider_operation_refused", "")
	})

	t.Run("close takes a host-refused hold with a pending qualification", func(t *testing.T) {
		req := billing.CloseProviderOperationParams{
			OperationID: "rejected", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(40_000), AttestedBy: "operator:paul", Reference: "invoice:rejected",
		}
		op, err := client.CloseProviderOperation(ctx, req)
		require.NoError(t, err)
		require.Equal(t, billing.ProviderBillingQualificationPending, op.Qualification.State)
		require.Equal(t, billing.ProviderBillingResolutionSettled, op.Resolution.Kind)
		require.Equal(t, billing.ProviderBillingObservationRejected, op.Refusal.Reason)
		require.Equal(t, billing.ProviderOperationSettled, op.State)
		require.EqualValues(t, 40_000, *op.SettlementAmount)
		var manifest struct {
			Provider              string          `json:"provider"`
			QualificationReason   string          `json:"qualification_reason"`
			BaselineObservationID string          `json:"baseline_observation_id"`
			RefusedObservation    json.RawMessage `json:"refused_observation"`
			Refusal               struct {
				Reason string `json:"reason"`
			} `json:"refusal"`
		}
		require.NoError(t, json.Unmarshal(op.SettlementBody, &manifest))
		require.Equal(t, "provider", manifest.Provider)
		require.Equal(t, "awaiting_equal_observation", manifest.QualificationReason)
		require.Equal(t, "rejected:1", manifest.BaselineObservationID)
		require.Equal(t, "null", string(manifest.RefusedObservation), "the qualifier never refused it")
		require.Equal(t, "observation_rejected", manifest.Refusal.Reason)
		bal := balance(evidence)
		require.EqualValues(t, 960_000, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.HeldAmount)

		replay, err := client.CloseProviderOperation(ctx, req)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Empty(t, list(stuck)["order"])
	})

	t.Run("a host transaction commits or rolls back the refusal", func(t *testing.T) {
		customer := fund(100_000)
		open(customer, "tx", 100_000)
		req := refuse("tx", "tx:host", billing.ProviderBillingRefusalUnavailable, "")
		tx, err := f.pool.Begin(ctx)
		require.NoError(t, err)
		seen, err := client.RecordProviderBillingObservationTx(ctx, tx, req)
		require.NoError(t, err)
		require.NotNil(t, seen.Refusal)
		require.NoError(t, tx.Rollback(ctx))
		after, err := client.GetProviderOperation(ctx, "tx")
		require.NoError(t, err)
		require.Nil(t, after.Refusal)

		tx, err = f.pool.Begin(ctx)
		require.NoError(t, err)
		_, err = client.RecordProviderBillingObservationTx(ctx, tx, req)
		require.NoError(t, err)
		closed, err := client.CloseProviderOperationTx(ctx, tx, billing.CloseProviderOperationParams{
			OperationID: "tx", Kind: billing.ProviderBillingResolutionWrittenOff, AttestedBy: "operator:paul", Reference: "ticket:tx",
		})
		require.NoError(t, err)
		require.Equal(t, billing.ProviderOperationReleased, closed.State)
		require.NoError(t, tx.Commit(ctx))
		after, err = client.GetProviderOperation(ctx, "tx")
		require.NoError(t, err)
		require.Equal(t, billing.ProviderBillingUnavailable, after.Refusal.Reason)
		require.Equal(t, billing.ProviderBillingResolutionWrittenOff, after.Resolution.Kind)
		require.EqualValues(t, 0, balance(customer).HeldAmount)
	})

	t.Run("the database keeps refusals immutable and the qualifier's on a refused qualification", func(t *testing.T) {
		merchantID := client.MerchantID().UUID()
		_, err := f.pool.Exec(ctx, `INSERT INTO `+table("cost_refusals")+` (merchant_id, operation_id, reason) VALUES ($1, 'live', 'provider_evidence_refused')`, merchantID)
		require.ErrorContains(t, err, "cost_refusals_qualification_fkey")
		_, err = f.pool.Exec(ctx, `INSERT INTO `+table("cost_refusals")+` (merchant_id, operation_id, reason) VALUES ($1, 'live', 'stuck')`, merchantID)
		require.ErrorContains(t, err, "cost_refusals_reason_check")
		_, err = f.pool.Exec(ctx, `INSERT INTO `+table("cost_refusals")+` (merchant_id, operation_id, reason) VALUES ($1, 'missing', 'lifecycle_unprovable')`, merchantID)
		require.ErrorContains(t, err, "cost_refusals_authorization_fkey")
		_, err = f.pool.Exec(ctx, `UPDATE `+table("cost_refusals")+` SET detail = 'edited' WHERE merchant_id = $1 AND operation_id = 'unprovable'`, merchantID)
		require.Error(t, err)
		_, err = f.pool.Exec(ctx, `DELETE FROM `+table("cost_resolutions")+` WHERE merchant_id = $1 AND operation_id = 'unprovable'`, merchantID)
		require.Error(t, err)
		_, err = f.pool.Exec(ctx, `DELETE FROM `+table("cost_refusals")+` WHERE merchant_id = $1 AND operation_id = 'unprovable'`, merchantID)
		require.Error(t, err)
	})
}

// The migration that introduces refusals gives every refused qualification its
// refusal before cost_resolutions' key moves onto them, so a database with
// resolved and stuck refused holds upgrades in place.
func TestProviderBillingRefusalBackfill(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	require.NotEmpty(t, dsn, "OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	f := &fixture{pool: pool, schema: "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
		pool.Close()
	})
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	before := fstest.MapFS{}
	for _, name := range files {
		if strings.HasSuffix(name, "_cost_refusals.up.sql") {
			break
		}
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		before[name] = &fstest.MapFile{Data: body}
	}
	cfg := f.config()
	cfg.DB = &openrails.DBConfig{URL: dsn}
	application, err := app.BootstrapWithOptions(t.Context(), &cfg, &app.BootstrapOptions{PGXPool: pool, Migrations: before})
	require.NoError(t, err)
	require.NoError(t, application.Close(t.Context()))

	table := func(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(t.Context(), sql, args...)
		require.NoError(t, err)
	}
	var merchantID, accountID uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `INSERT INTO `+table("merchants")+` (slug, status) VALUES ($1, 'active') RETURNING id`, "backfill-"+uuid.NewString()[:8]).Scan(&merchantID))
	customer := uuid.New()
	exec(`INSERT INTO `+table("customers")+` (merchant_id, id) VALUES ($1, $2)`, merchantID, customer)
	require.NoError(t, pool.QueryRow(t.Context(), `INSERT INTO `+table("ledger_accounts")+`
		(merchant_id, customer_id, account_type, currency, debits_must_not_exceed_credits, credits_must_not_exceed_debits)
		VALUES ($1, $2, 'customer_balance', 'USD', true, false) RETURNING id`, merchantID, customer).Scan(&accountID))
	refusedHold := func(id string) {
		exec(`INSERT INTO `+table("operation_authorizations")+`
			(operation_id, merchant_id, customer_id, record_owner, ledger_account_id, currency, amount, claim_reference,
			 authorization_body_bytes, authorization_body_digest)
			VALUES ($1, $2, $3, 'host', $4, 'USD', 1000, 'claim-' || $1, 'body'::bytea, sha256('body'::bytea))`, id, merchantID, customer, accountID)
		exec(`INSERT INTO `+table("cost_qualifications")+`
			(merchant_id, operation_id, provider, provider_resource_id, provider_lifetime_starts_at, provider_lifetime_ends_at, provider_absent_at,
			 provider_absence_reference, billing_stop_reference, windows_closed_at, windows_closed_reference,
			 lifecycle_evidence_bytes, lifecycle_evidence_digest, quiescence_seconds, state, reason, updated_at)
			VALUES ($1, $2, 'cloud', 'resource-' || $2, now() - interval '3 hours', now() - interval '2 hours', now() - interval '2 hours',
			        'absent', 'stopped', now() - interval '2 hours', 'closed', 'evidence'::bytea, sha256('evidence'::bytea), 60,
			        'refused', 'provider_evidence_refused', '2026-10-01T00:00:00Z')`, merchantID, id)
		exec(`INSERT INTO `+table("cost_observations")+`
			(merchant_id, operation_id, observation_id, normalized_query, query_starts_at, query_ends_at, raw_body_available, raw_body_bytes, raw_body_digest,
			 covers_lifetime, has_negative_record, refusal_kind, qualification_reason, observed_at)
			VALUES ($1, $2, $2 || ':1', 'q', now() - interval '3 hours', now() - interval '1 hour', false, ''::bytea, sha256(''::bytea),
			        false, false, 'response_too_large', 'provider_evidence_refused', now() - interval '30 minutes')`, merchantID, id)
	}
	refusedHold("resolved")
	refusedHold("stuck")
	exec(`INSERT INTO `+table("cost_resolutions")+` (merchant_id, operation_id, kind, attested_by, reference) VALUES ($1, 'resolved', 'written_off', 'operator:paul', 'ticket:1')`, merchantID)
	exec(`UPDATE `+table("operation_authorizations")+` SET state = 'released', terminal_reference = 'ticket:1', released_at = now()
		WHERE merchant_id = $1 AND operation_id = 'resolved'`, merchantID)

	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: pool})
	require.NoError(t, err, "the migration applies over resolved and stuck refused holds")
	require.NoError(t, client.Close(t.Context()))

	rows, err := pool.Query(t.Context(), `SELECT operation_id, reason, detail, refused_at FROM `+table("cost_refusals")+` WHERE merchant_id = $1 ORDER BY operation_id`, merchantID)
	require.NoError(t, err)
	type refusal struct {
		Operation, Reason, Detail string
		At                        time.Time
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[refusal])
	require.NoError(t, err)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	require.Len(t, got, 2)
	for i, op := range []string{"resolved", "stuck"} {
		require.Equal(t, op, got[i].Operation)
		require.Equal(t, "provider_evidence_refused", got[i].Reason)
		require.Equal(t, "observation "+op+":1", got[i].Detail)
		require.True(t, at.Equal(got[i].At), "a refusal dates from its qualification's refusal")
	}
	_, err = pool.Exec(t.Context(), `INSERT INTO `+table("cost_resolutions")+` (merchant_id, operation_id, kind, attested_by, reference) VALUES ($1, 'unknown', 'written_off', 'operator:paul', 'ticket:2')`, merchantID)
	require.ErrorContains(t, err, "cost_resolutions_refusal_fkey")
}
