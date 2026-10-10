//go:build e2e && integration

package ci_test

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// A refused operation keeps its hold until an operator closes it: settled
// charges the attested cost at pass-through (any excess owed), written_off
// releases it. Closes replay, conflict on a changed term, refuse pending
// evidence, and the database admits them only for a refused hold.
func TestProviderBillingResolution(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "resolve-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	table := func(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }

	fund := func(amount int64) billing.CustomerID {
		customer := billing.CustomerID(uuid.New())
		_, err := createCreditGrant(ctx, client, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: amount, Source: "support", SourceID: uuid.NewString()})
		require.NoError(t, err)
		return customer
	}
	open := func(customer billing.CustomerID, operationID string, amount int64) {
		body := []byte(`{"rental":"` + operationID + `"}`)
		_, err := client.OpenProviderOperation(ctx, billing.OpenProviderOperationParams{
			OperationID: operationID, CustomerID: customer, RecordOwner: "user:1", Currency: "USD", Amount: amount,
			ClaimReference: "claim:" + operationID, AuthorizationBody: body, AuthorizationBodySHA256: billing.SHA256(sha256.Sum256(body)),
		})
		require.NoError(t, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	start, end := now.Add(-2*time.Hour), now.Add(-time.Hour)
	observation := func(operationID, observationID string) billing.RecordProviderBillingObservationParams {
		return billing.RecordProviderBillingObservationParams{
			OperationID: operationID, ObservationID: observationID,
			Lifecycle: billing.ProviderBillingLifecycleEvidence{
				Provider: "provider", ProviderResourceID: "pod-" + operationID,
				ProviderLifetimeStartsAt: start, ProviderLifetimeEndsAt: end, ProviderAbsentAt: end,
				ProviderAbsenceReference: "absence:1", BillingStopReference: "stop:1",
				WindowsClosedAt: end, WindowsClosedReference: "windows:1", LifecycleEvidenceBody: []byte(`{"absent":true}`),
			},
			NormalizedQuery: "pod=" + operationID, QueryStartsAt: start, QueryEndsAt: end,
		}
	}
	record := func(in billing.RecordProviderBillingObservationParams, costs ...int64) *billing.ProviderOperation {
		t.Helper()
		if in.Refusal == nil {
			in.RawBody = []byte(`[{"amount":1}]`)
			for _, cost := range costs {
				in.Records = append(in.Records, billing.ProviderBillingRecord{ProviderResourceID: "pod-" + in.OperationID, BucketStart: start, Amount: cost, TimeBilledMS: 60_000})
			}
		}
		op, err := client.RecordProviderBillingObservation(ctx, in)
		require.NoError(t, err)
		return op
	}
	tooLarge := func(operationID string) {
		in := observation(operationID, operationID+":1")
		in.Refusal = &billing.ProviderBillingObservationRefusal{Kind: billing.ProviderBillingRefusalResponseTooLarge}
		require.Equal(t, billing.ProviderBillingQualificationRefused, record(in).Qualification.State)
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
	resolve := func(req billing.CloseProviderOperationParams) (*billing.ProviderOperation, error) {
		return client.CloseProviderOperation(ctx, req)
	}
	list := func(params billing.ProviderOperationListParams) []string {
		t.Helper()
		var ids []string
		for {
			page, err := client.ListProviderOperations(ctx, params)
			require.NoError(t, err)
			for _, q := range page.Items {
				ids = append(ids, q.OperationID)
			}
			if page.Next == "" {
				return ids
			}
			params.Cursor = page.Next
		}
	}
	yes := true
	stuck := billing.ProviderOperationListParams{State: []billing.ProviderOperationState{billing.ProviderOperationOpen}, Refused: &yes}

	settler, writeOff, zero := fund(1_000_000), fund(1_000_000), fund(1_000_000)
	open(settler, "too-large", 400_000)
	open(writeOff, "negative", 300_000)
	open(zero, "falling", 200_000)
	open(zero, "pending", 100_000)
	open(zero, "no-evidence", 100_000)
	tooLarge("too-large")
	time.Sleep(time.Millisecond)
	negative := record(observation("negative", "negative:1"), 500, -100)
	require.Equal(t, billing.ProviderBillingNegativeOrCorrective, negative.Qualification.Reason)
	time.Sleep(time.Millisecond)
	require.Equal(t, billing.ProviderBillingAwaitingEqualObservation, record(observation("falling", "falling:1"), 900).Qualification.Reason)
	falling := record(observation("falling", "falling:2"), 400)
	require.Equal(t, billing.ProviderBillingDecreasingProviderCost, falling.Qualification.Reason)
	time.Sleep(time.Millisecond)
	require.Equal(t, billing.ProviderBillingQualificationPending, record(observation("pending", "pending:1"), 50).Qualification.State)

	t.Run("the list finds the stuck holds", func(t *testing.T) {
		require.Equal(t, []string{"falling", "negative", "too-large"}, list(stuck))
		page, err := client.ListProviderOperations(ctx, stuck)
		require.NoError(t, err)
		reasons := map[string]string{}
		for _, op := range page.Items {
			require.NotNil(t, op.Refusal, op.OperationID)
			require.Equal(t, billing.ProviderBillingQualificationRefused, op.Qualification.State, op.OperationID)
			reasons[op.OperationID] = string(op.Refusal.Reason) + " / " + op.Refusal.Detail
		}
		require.Equal(t, map[string]string{
			"falling":   "decreasing_provider_cost / observation falling:2",
			"negative":  "negative_or_corrective_record / observation negative:1",
			"too-large": "provider_evidence_refused / observation too-large:1: response_too_large",
		}, reasons, "the qualifier refuses the hold with its reason")
		paged := stuck
		paged.Limit = 1
		require.Equal(t, []string{"falling", "negative", "too-large"}, list(paged))
		require.Equal(t, []string{"no-evidence", "pending", "falling", "negative", "too-large"}, list(billing.ProviderOperationListParams{}))
		_, err = client.ListProviderOperations(ctx, billing.ProviderOperationListParams{State: []billing.ProviderOperationState{"closed"}})
		refused(err, 400, "invalid_query", "state")
		_, err = client.ListProviderOperations(ctx, billing.ProviderOperationListParams{PageRequest: billing.PageRequest{Cursor: "not-a-cursor"}})
		refused(err, 400, "invalid_cursor", "cursor")
	})

	t.Run("refusals", func(t *testing.T) {
		settle := billing.CloseProviderOperationParams{OperationID: "too-large", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(1), AttestedBy: "operator:paul", Reference: "invoice:1"}
		for _, bad := range []func(*billing.CloseProviderOperationParams){
			func(r *billing.CloseProviderOperationParams) { r.CostAmount = nil },
			func(r *billing.CloseProviderOperationParams) { r.CostAmount = cost(-1) },
			func(r *billing.CloseProviderOperationParams) {
				r.Kind = billing.ProviderBillingResolutionWrittenOff
			},
			func(r *billing.CloseProviderOperationParams) { r.Kind = "refunded" },
			func(r *billing.CloseProviderOperationParams) { r.AttestedBy = "" },
			func(r *billing.CloseProviderOperationParams) { r.Reference = " invoice:1" },
			func(r *billing.CloseProviderOperationParams) { r.Note = "note " },
		} {
			req := settle
			bad(&req)
			_, err := resolve(req)
			refused(err, 400, "invalid_param", "")
		}
		req := settle
		req.OperationID = "pending"
		_, err := resolve(req)
		refused(err, 409, "provider_operation_not_refused", "")
		req.OperationID = "no-evidence"
		_, err = resolve(req)
		refused(err, 409, "provider_operation_not_refused", "")
		req.OperationID = "missing"
		_, err = resolve(req)
		refused(err, 404, "provider_operation_not_found", "")
		require.Equal(t, []string{"falling", "negative", "too-large"}, list(stuck), "a refusal writes nothing")
	})

	t.Run("settled charges the attested cost above the hold as owed", func(t *testing.T) {
		req := billing.CloseProviderOperationParams{
			OperationID: "too-large", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(1_500_000),
			AttestedBy: "operator:paul", Reference: "runpod-invoice:2026-10", Note: "billing history exceeded the observation envelope",
		}
		auth, err := resolve(req)
		require.NoError(t, err)
		require.False(t, auth.Replayed)
		require.Equal(t, billing.ProviderBillingQualificationRefused, auth.Qualification.State)
		require.Equal(t, billing.ProviderBillingProviderEvidenceRefused, auth.Qualification.Reason)
		require.NotNil(t, auth.Resolution)
		require.Equal(t, billing.ProviderBillingResolutionSettled, auth.Resolution.Kind)
		require.EqualValues(t, 1_500_000, *auth.Resolution.CostAmount)
		require.Equal(t, "operator:paul", auth.Resolution.AttestedBy)
		require.Equal(t, billing.ProviderOperationSettled, auth.State)
		require.EqualValues(t, 1_500_000, *auth.SettlementCostAmount)
		require.EqualValues(t, 1_500_000, *auth.SettlementAmount)
		require.Equal(t, "sha256:"+auth.SettlementBodySHA256.String(), auth.TerminalReference)
		var manifest struct {
			Contract            string `json:"contract"`
			QualificationReason string `json:"qualification_reason"`
			RefusedObservation  struct {
				ObservationID string  `json:"observation_id"`
				RefusalKind   *string `json:"refusal_kind"`
				RawBodySHA256 string  `json:"raw_body_sha256"`
			} `json:"refused_observation"`
			Authorization struct {
				AuthorizedAmount string `json:"authorized_amount"`
			} `json:"authorization"`
			CostAmount string `json:"cost_amount"`
			AttestedBy string `json:"attested_by"`
			Reference  string `json:"reference"`
			Note       string `json:"note"`
		}
		require.NoError(t, json.Unmarshal(auth.SettlementBody, &manifest))
		require.Equal(t, "openrails/operator-attested-provider-cost", manifest.Contract)
		require.Equal(t, "provider_evidence_refused", manifest.QualificationReason)
		require.Equal(t, "too-large:1", manifest.RefusedObservation.ObservationID)
		require.Equal(t, "response_too_large", *manifest.RefusedObservation.RefusalKind)
		empty := sha256.Sum256(nil)
		require.Equal(t, billing.SHA256(empty).String(), manifest.RefusedObservation.RawBodySHA256)
		require.Equal(t, "400000", manifest.Authorization.AuthorizedAmount)
		require.Equal(t, "1500000", manifest.CostAmount)
		require.Equal(t, "runpod-invoice:2026-10", manifest.Reference)
		require.Equal(t, req.Note, manifest.Note)

		bal := balance(settler)
		require.EqualValues(t, 0, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.HeldAmount)
		require.EqualValues(t, 500_000, bal.OwedAmount)

		replay, err := resolve(req)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Equal(t, auth.TerminalReference, replay.TerminalReference)
		require.Equal(t, bal, balance(settler), "a replay charges nothing")
		changed := req
		changed.CostAmount = cost(1_400_000)
		_, err = resolve(changed)
		refused(err, 409, "provider_operation_conflict", "cost_amount")
		changed = req
		changed.Kind, changed.CostAmount = billing.ProviderBillingResolutionWrittenOff, nil
		_, err = resolve(changed)
		refused(err, 409, "provider_operation_conflict", "kind")
		changed = req
		changed.Note = ""
		_, err = resolve(changed)
		refused(err, 409, "provider_operation_conflict", "note")

		got, err := client.GetProviderOperation(ctx, "too-large")
		require.NoError(t, err)
		require.Equal(t, auth.Resolution, got.Resolution)
		require.Equal(t, auth.Qualification, got.Qualification)
		late := observation("too-large", "too-large:2")
		late.RawBody, late.Records = []byte(`[]`), []billing.ProviderBillingRecord{}
		_, err = client.RecordProviderBillingObservation(ctx, late)
		refused(err, 409, "provider_operation_not_open", "")
	})

	t.Run("written_off releases the hold uncharged", func(t *testing.T) {
		req := billing.CloseProviderOperationParams{
			OperationID: "negative", Kind: billing.ProviderBillingResolutionWrittenOff, AttestedBy: "operator:paul", Reference: "ticket:42",
		}
		require.EqualValues(t, 300_000, balance(writeOff).HeldAmount)
		op, err := resolve(req)
		require.NoError(t, err)
		require.Equal(t, billing.ProviderBillingResolutionWrittenOff, op.Resolution.Kind)
		require.Nil(t, op.Resolution.CostAmount)
		require.Equal(t, billing.ProviderOperationReleased, op.State)
		require.Equal(t, "ticket:42", op.TerminalReference)
		require.Nil(t, op.SettlementAmount)
		bal := balance(writeOff)
		require.EqualValues(t, 1_000_000, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.HeldAmount)
		require.EqualValues(t, 0, bal.OwedAmount)
		replay, err := resolve(req)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		_, err = client.ReleaseProviderOperation(ctx, billing.ReleaseProviderOperationParams{OperationID: "negative", ReleaseReference: "ticket:42"})
		refused(err, 409, "provider_operation_has_billing_evidence", "")
	})

	t.Run("a host transaction commits or rolls back the resolution", func(t *testing.T) {
		req := billing.CloseProviderOperationParams{
			OperationID: "falling", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(0), AttestedBy: "operator:paul", Reference: "invoice:zero",
		}
		tx, err := f.pool.Begin(ctx)
		require.NoError(t, err)
		seen, err := client.CloseProviderOperationTx(ctx, tx, req)
		require.NoError(t, err)
		require.Equal(t, billing.ProviderOperationSettled, seen.State)
		require.NoError(t, tx.Rollback(ctx))
		after, err := client.GetProviderOperation(ctx, "falling")
		require.NoError(t, err)
		require.Nil(t, after.Resolution)
		require.Equal(t, billing.ProviderOperationOpen, after.State)

		tx, err = f.pool.Begin(ctx)
		require.NoError(t, err)
		_, err = client.CloseProviderOperationTx(ctx, tx, req)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		after, err = client.GetProviderOperation(ctx, "falling")
		require.NoError(t, err)
		require.EqualValues(t, 0, *after.SettlementAmount, "a zero attested cost settles at zero")
		bal := balance(zero)
		require.EqualValues(t, 1_000_000, bal.BalanceAmount)
		require.EqualValues(t, 200_000, bal.HeldAmount, "pending and no-evidence still hold")
		require.Empty(t, list(stuck))
	})

	t.Run("concurrent resolutions record one", func(t *testing.T) {
		customer := fund(1_000_000)
		open(customer, "race", 100_000)
		tooLarge("race")
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, req := range []billing.CloseProviderOperationParams{
			{OperationID: "race", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(70_000), AttestedBy: "operator:a", Reference: "invoice:a"},
			{OperationID: "race", Kind: billing.ProviderBillingResolutionWrittenOff, AttestedBy: "operator:b", Reference: "ticket:b"},
		} {
			wg.Go(func() { _, err := resolve(req); errs <- err })
		}
		wg.Wait()
		close(errs)
		var won, lost int
		for err := range errs {
			if err == nil {
				won++
				continue
			}
			require.ErrorIs(t, err, billing.ErrProviderOperationConflict)
			lost++
		}
		require.Equal(t, 1, won)
		require.Equal(t, 1, lost)
		require.EqualValues(t, 0, balance(customer).HeldAmount)
	})

	t.Run("the database admits a resolution only for a refused hold", func(t *testing.T) {
		merchantID := client.MerchantID().UUID()
		_, err := f.pool.Exec(ctx, `INSERT INTO `+table("cost_resolutions")+` (merchant_id, operation_id, kind, attested_by, reference)
			VALUES ($1, 'pending', 'written_off', 'operator:sql', 'ticket:sql')`, merchantID)
		require.ErrorContains(t, err, "cost_resolutions_refusal_fkey")
		_, err = f.pool.Exec(ctx, `UPDATE `+table("cost_qualifications")+` SET state = 'pending', reason = 'awaiting_equal_observation'
			WHERE merchant_id = $1 AND operation_id = 'negative'`, merchantID)
		require.ErrorContains(t, err, "cost_refusals_qualification_fkey")
		_, err = f.pool.Exec(ctx, `UPDATE `+table("cost_resolutions")+` SET reference = 'ticket:43' WHERE merchant_id = $1 AND operation_id = 'negative'`, merchantID)
		require.Error(t, err)
		_, err = f.pool.Exec(ctx, `DELETE FROM `+table("cost_resolutions")+` WHERE merchant_id = $1 AND operation_id = 'negative'`, merchantID)
		require.Error(t, err)
	})
}
