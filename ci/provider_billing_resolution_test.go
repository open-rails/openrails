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

// A refused provider billing qualification keeps its hold until an operator
// closes it: the list finds the stuck holds, a settled resolution charges the
// attested cost at pass-through (above the hold as owed), a written_off one
// releases the hold uncharged. Resolutions replay, conflict on a changed term,
// refuse pending evidence, commit with a host transaction, and the database
// admits them only for a refused qualification that then stays refused.
func TestProviderBillingResolution(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "resolve-"+uuid.NewString()[:8])
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	table := func(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }

	fund := func(amount int64) billing.CustomerID {
		customer := billing.CustomerID(uuid.New())
		_, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: customer}})
		require.NoError(t, err)
		_, err = client.CreateCreditGrant(ctx, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: amount, Source: "support", SourceID: uuid.NewString()})
		require.NoError(t, err)
		return customer
	}
	open := func(customer billing.CustomerID, operationID string, amount int64) {
		body := []byte(`{"rental":"` + operationID + `"}`)
		_, err := client.OpenOperationAuthorization(ctx, billing.OpenOperationAuthorizationParams{
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
	record := func(in billing.RecordProviderBillingObservationParams, costs ...int64) *billing.ProviderBillingQualification {
		t.Helper()
		if in.Refusal == nil {
			in.RawBody = []byte(`[{"amount":1}]`)
			for _, cost := range costs {
				in.Records = append(in.Records, billing.ProviderBillingRecord{ProviderResourceID: "pod-" + in.OperationID, BucketStart: start, Amount: cost, TimeBilledMS: 60_000})
			}
		}
		qual, err := client.RecordProviderBillingObservation(ctx, in)
		require.NoError(t, err)
		return qual
	}
	tooLarge := func(operationID string) {
		in := observation(operationID, operationID+":1")
		in.Refusal = &billing.ProviderBillingObservationRefusal{Kind: billing.ProviderBillingRefusalResponseTooLarge}
		require.Equal(t, billing.ProviderBillingQualificationRefused, record(in).State)
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
	resolve := func(req billing.ResolveProviderBillingQualificationParams) (*billing.ProviderBillingQualification, error) {
		return client.ResolveProviderBillingQualification(ctx, req)
	}
	list := func(params billing.ProviderBillingQualificationListParams) []string {
		t.Helper()
		var ids []string
		for {
			page, err := client.ListProviderBillingQualifications(ctx, params)
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
	stuck := billing.ProviderBillingQualificationListParams{
		State:              []billing.ProviderBillingQualificationState{billing.ProviderBillingQualificationRefused},
		AuthorizationState: []billing.OperationAuthorizationState{billing.OperationAuthorizationOpen},
	}

	settler, writeOff, zero := fund(1_000_000), fund(1_000_000), fund(1_000_000)
	open(settler, "too-large", 400_000)
	open(writeOff, "negative", 300_000)
	open(zero, "falling", 200_000)
	open(zero, "pending", 100_000)
	open(zero, "no-evidence", 100_000)
	tooLarge("too-large")
	time.Sleep(time.Millisecond)
	negative := record(observation("negative", "negative:1"), 500, -100)
	require.Equal(t, billing.ProviderBillingNegativeOrCorrective, negative.Reason)
	time.Sleep(time.Millisecond)
	require.Equal(t, billing.ProviderBillingAwaitingEqualObservation, record(observation("falling", "falling:1"), 900).Reason)
	falling := record(observation("falling", "falling:2"), 400)
	require.Equal(t, billing.ProviderBillingDecreasingProviderCost, falling.Reason)
	time.Sleep(time.Millisecond)
	require.Equal(t, billing.ProviderBillingQualificationPending, record(observation("pending", "pending:1"), 50).State)

	t.Run("the list finds the stuck holds", func(t *testing.T) {
		require.Equal(t, []string{"falling", "negative", "too-large"}, list(stuck))
		paged := stuck
		paged.Limit = 1
		require.Equal(t, []string{"falling", "negative", "too-large"}, list(paged))
		require.Equal(t, []string{"pending", "falling", "negative", "too-large"}, list(billing.ProviderBillingQualificationListParams{}))
		require.Equal(t, []string{"pending"}, list(billing.ProviderBillingQualificationListParams{State: []billing.ProviderBillingQualificationState{billing.ProviderBillingQualificationPending}}))
		_, err := client.ListProviderBillingQualifications(ctx, billing.ProviderBillingQualificationListParams{State: []billing.ProviderBillingQualificationState{"stuck"}})
		refused(err, 400, "invalid_query", "state")
		_, err = client.ListProviderBillingQualifications(ctx, billing.ProviderBillingQualificationListParams{AuthorizationState: []billing.OperationAuthorizationState{"closed"}})
		refused(err, 400, "invalid_query", "authorization_state")
		_, err = client.ListProviderBillingQualifications(ctx, billing.ProviderBillingQualificationListParams{PageRequest: billing.PageRequest{Cursor: "not-a-cursor"}})
		refused(err, 400, "invalid_cursor", "cursor")
	})

	t.Run("refusals", func(t *testing.T) {
		settle := billing.ResolveProviderBillingQualificationParams{OperationID: "too-large", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(1), AttestedBy: "operator:paul", Reference: "invoice:1"}
		for _, bad := range []func(*billing.ResolveProviderBillingQualificationParams){
			func(r *billing.ResolveProviderBillingQualificationParams) { r.CostAmount = nil },
			func(r *billing.ResolveProviderBillingQualificationParams) { r.CostAmount = cost(-1) },
			func(r *billing.ResolveProviderBillingQualificationParams) {
				r.Kind = billing.ProviderBillingResolutionWrittenOff
			},
			func(r *billing.ResolveProviderBillingQualificationParams) { r.Kind = "refunded" },
			func(r *billing.ResolveProviderBillingQualificationParams) { r.AttestedBy = "" },
			func(r *billing.ResolveProviderBillingQualificationParams) { r.Reference = " invoice:1" },
			func(r *billing.ResolveProviderBillingQualificationParams) { r.Note = "note " },
		} {
			req := settle
			bad(&req)
			_, err := resolve(req)
			refused(err, 400, "invalid_param", "")
		}
		req := settle
		req.OperationID = "pending"
		_, err := resolve(req)
		refused(err, 409, "provider_billing_qualification_not_refused", "")
		req.OperationID = "no-evidence"
		_, err = resolve(req)
		refused(err, 404, "provider_billing_qualification_not_found", "")
		req.OperationID = "missing"
		_, err = resolve(req)
		refused(err, 404, "operation_authorization_not_found", "")
		require.Equal(t, []string{"falling", "negative", "too-large"}, list(stuck), "a refusal writes nothing")
	})

	t.Run("settled charges the attested cost above the hold as owed", func(t *testing.T) {
		req := billing.ResolveProviderBillingQualificationParams{
			OperationID: "too-large", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(1_500_000),
			AttestedBy: "operator:paul", Reference: "runpod-invoice:2026-10", Note: "billing history exceeded the observation envelope",
		}
		qual, err := resolve(req)
		require.NoError(t, err)
		require.False(t, qual.Replayed)
		require.Equal(t, billing.ProviderBillingQualificationRefused, qual.State)
		require.Equal(t, billing.ProviderBillingProviderEvidenceRefused, qual.Reason)
		require.NotNil(t, qual.Resolution)
		require.Equal(t, billing.ProviderBillingResolutionSettled, qual.Resolution.Kind)
		require.EqualValues(t, 1_500_000, *qual.Resolution.CostAmount)
		require.Equal(t, "operator:paul", qual.Resolution.AttestedBy)
		auth := qual.Authorization
		require.Equal(t, billing.OperationAuthorizationSettled, auth.State)
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
		require.Equal(t, auth.TerminalReference, replay.Authorization.TerminalReference)
		require.Equal(t, bal, balance(settler), "a replay charges nothing")
		changed := req
		changed.CostAmount = cost(1_400_000)
		_, err = resolve(changed)
		refused(err, 409, "provider_billing_resolution_conflict", "cost_amount")
		changed = req
		changed.Kind, changed.CostAmount = billing.ProviderBillingResolutionWrittenOff, nil
		_, err = resolve(changed)
		refused(err, 409, "provider_billing_resolution_conflict", "kind")
		changed = req
		changed.Note = ""
		_, err = resolve(changed)
		refused(err, 409, "provider_billing_resolution_conflict", "note")

		got, err := client.GetProviderBillingQualification(ctx, "too-large")
		require.NoError(t, err)
		require.Equal(t, qual.Resolution, got.Resolution)
		late := observation("too-large", "too-large:2")
		late.RawBody, late.Records = []byte(`[]`), []billing.ProviderBillingRecord{}
		_, err = client.RecordProviderBillingObservation(ctx, late)
		refused(err, 409, "operation_authorization_not_open", "")
	})

	t.Run("written_off releases the hold uncharged", func(t *testing.T) {
		req := billing.ResolveProviderBillingQualificationParams{
			OperationID: "negative", Kind: billing.ProviderBillingResolutionWrittenOff, AttestedBy: "operator:paul", Reference: "ticket:42",
		}
		require.EqualValues(t, 300_000, balance(writeOff).HeldAmount)
		qual, err := resolve(req)
		require.NoError(t, err)
		require.Equal(t, billing.ProviderBillingResolutionWrittenOff, qual.Resolution.Kind)
		require.Nil(t, qual.Resolution.CostAmount)
		require.Equal(t, billing.OperationAuthorizationReleased, qual.Authorization.State)
		require.Equal(t, "ticket:42", qual.Authorization.TerminalReference)
		require.Nil(t, qual.Authorization.SettlementAmount)
		bal := balance(writeOff)
		require.EqualValues(t, 1_000_000, bal.BalanceAmount)
		require.EqualValues(t, 0, bal.HeldAmount)
		require.EqualValues(t, 0, bal.OwedAmount)
		replay, err := resolve(req)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		_, err = client.ReleaseOperationAuthorization(ctx, billing.ReleaseOperationAuthorizationParams{OperationID: "negative", ReleaseReference: "ticket:42"})
		refused(err, 409, "operation_authorization_has_billing_evidence", "")
	})

	t.Run("a host transaction commits or rolls back the resolution", func(t *testing.T) {
		req := billing.ResolveProviderBillingQualificationParams{
			OperationID: "falling", Kind: billing.ProviderBillingResolutionSettled, CostAmount: cost(0), AttestedBy: "operator:paul", Reference: "invoice:zero",
		}
		tx, err := f.pool.Begin(ctx)
		require.NoError(t, err)
		seen, err := client.ResolveProviderBillingQualificationTx(ctx, tx, req)
		require.NoError(t, err)
		require.Equal(t, billing.OperationAuthorizationSettled, seen.Authorization.State)
		require.NoError(t, tx.Rollback(ctx))
		after, err := client.GetProviderBillingQualification(ctx, "falling")
		require.NoError(t, err)
		require.Nil(t, after.Resolution)
		require.Equal(t, billing.OperationAuthorizationOpen, after.Authorization.State)

		tx, err = f.pool.Begin(ctx)
		require.NoError(t, err)
		_, err = client.ResolveProviderBillingQualificationTx(ctx, tx, req)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		after, err = client.GetProviderBillingQualification(ctx, "falling")
		require.NoError(t, err)
		require.EqualValues(t, 0, *after.Authorization.SettlementAmount, "a zero attested cost settles at zero")
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
		for _, req := range []billing.ResolveProviderBillingQualificationParams{
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
			require.ErrorIs(t, err, billing.ErrProviderBillingResolutionConflict)
			lost++
		}
		require.Equal(t, 1, won)
		require.Equal(t, 1, lost)
		require.EqualValues(t, 0, balance(customer).HeldAmount)
	})

	t.Run("the database admits a resolution only for a refused qualification", func(t *testing.T) {
		merchantID := client.MerchantID().UUID()
		_, err := f.pool.Exec(ctx, `INSERT INTO `+table("cost_resolutions")+` (merchant_id, operation_id, kind, attested_by, reference)
			VALUES ($1, 'pending', 'written_off', 'operator:sql', 'ticket:sql')`, merchantID)
		require.ErrorContains(t, err, "cost_resolutions_qualification_fkey")
		_, err = f.pool.Exec(ctx, `UPDATE `+table("cost_qualifications")+` SET state = 'pending', reason = 'awaiting_equal_observation'
			WHERE merchant_id = $1 AND operation_id = 'negative'`, merchantID)
		require.ErrorContains(t, err, "cost_resolutions_qualification_fkey")
		_, err = f.pool.Exec(ctx, `UPDATE `+table("cost_resolutions")+` SET reference = 'ticket:43' WHERE merchant_id = $1 AND operation_id = 'negative'`, merchantID)
		require.Error(t, err)
		_, err = f.pool.Exec(ctx, `DELETE FROM `+table("cost_resolutions")+` WHERE merchant_id = $1 AND operation_id = 'negative'`, merchantID)
		require.Error(t, err)
	})
}
