//go:build integration

package embed_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/pkg/embedded"
)

const providerBillingTestQuiescence = time.Second

func withProviderBillingQuiescence(cfg *config.Config) {
	cfg.ProviderBillingQuiescenceInterval = providerBillingTestQuiescence.String()
}

func newProviderObligationRuntime(t *testing.T, ctx context.Context, h *integrationharness.Harness) *embed.Runtime {
	t.Helper()
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}}
	withProviderBillingQuiescence(cfg)
	runtime, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, Redis: h.Redis, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	runtime.Embedded().App().Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)
	return runtime
}

type providerFixture struct {
	h      *integrationharness.Harness
	client *openrails.Client
	payer  openrails.CustomerID
	start  time.Time
}

func newProviderFixture(t *testing.T, ctx context.Context, h *integrationharness.Harness, client *openrails.Client, funded int64) *providerFixture {
	t.Helper()
	payer := uuid.New()
	_, err := h.Pool().Exec(ctx, `INSERT INTO openrails.customers (id, merchant_id, issuer, created_at, last_seen_at)
		VALUES ($1, $2, 'provider-obligations', now(), now())`, payer, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	id := openrails.CustomerID(payer)
	_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: &id, Invoker: "provider-obligations", Currency: "USD", Amount: funded,
		Source: "provider-obligations", SourceID: uuid.NewString(),
	})
	require.NoError(t, err)
	return &providerFixture{h: h, client: client, payer: id, start: time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)}
}

func (f *providerFixture) authorization(label string, amount int64) openrails.OperationAuthorizationRequest {
	operationID := label + "-" + uuid.NewString() + "/create"
	body := []byte(`{"format":"th-auth-v1","operation_id":"` + operationID + `"}`)
	return openrails.OperationAuthorizationRequest{
		OperationID: operationID, Payer: f.payer, RecordOwner: "user:" + f.payer.String(),
		AuthorizedUSDMicros: amount, ClaimReference: "claim:" + uuid.NewString(),
		AuthorizationBody: body, AuthorizationBodySHA256: sha256.Sum256(body),
	}
}

func (f *providerFixture) observation(operationID, observationID string, costs ...int64) openrails.ProviderBillingObservationRequest {
	records := make([]openrails.ProviderBillingRecord, len(costs))
	for i, cost := range costs {
		records[i] = openrails.ProviderBillingRecord{ProviderResourceID: "pod-" + operationID, BucketStart: f.start.Add(time.Duration(i) * time.Minute), AmountUSDMicros: cost, TimeBilledMS: 60_000}
	}
	return openrails.ProviderBillingObservationRequest{
		OperationID: operationID, ObservationID: observationID,
		Lifecycle: openrails.ProviderBillingLifecycleEvidence{
			Provider: "runpod", ProviderResourceID: "pod-" + operationID,
			ProviderLifetimeStart: f.start, ProviderLifetimeEnd: f.start.Add(time.Hour),
			ProviderAbsentAt: f.start.Add(time.Hour + time.Minute), ProviderAbsenceReference: "absence:" + operationID,
			BillingStopReference: "billing-stop:" + operationID, WindowsClosedAt: f.start.Add(time.Hour),
			WindowsClosedReference: "windows-closed:" + operationID, LifecycleEvidenceBody: []byte(`{"provider_absent":true}`),
		},
		NormalizedQuery: "grouping=podId&podId=pod-" + operationID,
		QueryStart:      f.start.Add(-time.Minute), QueryEnd: f.start.Add(90 * time.Minute),
		RawBody: []byte(fmt.Sprintf(`{"costs":%v}`, costs)), Records: records,
	}
}

func (f *providerFixture) account(t *testing.T, ctx context.Context) string {
	t.Helper()
	account, err := f.client.GetCreditAccount(ctx, f.payer.String(), "USD")
	require.NoError(t, err)
	return fmt.Sprintf("balance=%d held=%d available=%d owed=%d", account.BalanceAmount, account.HeldAmount, account.AvailableAmount, account.OutstandingOwedAmount)
}

func describeAuthorization(label string, auth *openrails.OperationAuthorization, err error) string {
	if err != nil {
		return label + ": " + describeProviderError(err)
	}
	if auth == nil {
		return label + ": unexpected success"
	}
	line := fmt.Sprintf("%s: state=%s replayed=%t authorized=%d terminal=%t", label, auth.State, auth.Replayed, auth.AuthorizedUSDMicros, auth.TerminalReference != "")
	if auth.State == openrails.OperationAuthorizationSettled {
		line += fmt.Sprintf(" cost=%d rated=%d body_digest_ok=%t", *auth.SettlementProviderCostUSDMicros, *auth.SettlementRatedUSDMicros,
			auth.SettlementBodySHA256 != nil && *auth.SettlementBodySHA256 == sha256.Sum256(auth.SettlementBody))
	}
	return line
}

func describeQualification(label string, q *openrails.ProviderBillingQualification, err error) string {
	if err != nil {
		return label + ": " + describeProviderError(err)
	}
	if q == nil {
		return label + ": unexpected success"
	}
	cost := "null"
	if q.QualifiedProviderCostUSDMicros != nil {
		cost = fmt.Sprint(*q.QualifiedProviderCostUSDMicros)
	}
	return fmt.Sprintf("%s: state=%s reason=%s replayed=%t qualified_cost=%s quiescence=%d | %s", label, q.State, q.Reason, q.Replayed, cost, q.QuiescenceSeconds, describeAuthorization("authorization", &q.Authorization, nil))
}

// describeProviderError records the HTTP response for a server error and the
// error class alone for a request the Client refuses before sending.
func describeProviderError(err error) string {
	head := "local"
	var status *openrails.StatusError
	if errors.As(err, &status) {
		param := ""
		if status.Param != nil {
			param = *status.Param
		}
		head = fmt.Sprintf("status=%d code=%s param=%s", status.Status, status.Code, param)
	}
	return fmt.Sprintf("%s invalid=%t not_found=%t conflict=%t insufficient=%t", head,
		errors.Is(err, openrails.ErrInvalid), errors.Is(err, openrails.ErrNotFound), errors.Is(err, openrails.ErrConflict), errors.Is(err, openrails.ErrInsufficientCredits))
}

// providerObligationWorkflow drives every provider-obligation command through
// one Client and returns a deployment-neutral transcript.
func providerObligationWorkflow(t *testing.T, ctx context.Context, f *providerFixture) []string {
	c := f.client
	var out []string
	add := func(line string) { out = append(out, line) }

	a := f.authorization("reserve", 6_000)
	auth, err := c.OpenOperationAuthorization(ctx, a)
	add(describeAuthorization("open", auth, err))
	auth, err = c.OpenOperationAuthorization(ctx, a)
	add(describeAuthorization("open replay", auth, err))
	changed := a
	changed.AuthorizationBody = []byte(`{"changed":true}`)
	changed.AuthorizationBodySHA256 = sha256.Sum256(changed.AuthorizationBody)
	_, err = c.OpenOperationAuthorization(ctx, changed)
	add(describeAuthorization("open changed body", nil, err))
	mismatched := f.authorization("digest", 1)
	mismatched.AuthorizationBodySHA256[0] ^= 0xff
	_, err = c.OpenOperationAuthorization(ctx, mismatched)
	add(describeAuthorization("open digest mismatch", nil, err))
	_, err = c.OpenOperationAuthorization(ctx, f.authorization("over-capacity", 5_000))
	add(describeAuthorization("open over capacity", nil, err))
	add("account after open: " + f.account(t, ctx))
	auth, err = c.GetOperationAuthorization(ctx, a.OperationID)
	add(describeAuthorization("get", auth, err))
	_, err = c.GetOperationAuthorization(ctx, "missing-"+uuid.NewString())
	add(describeAuthorization("get missing", nil, err))
	_, err = c.GetOperationAuthorization(ctx, "..")
	add(describeAuthorization("get dot segment", nil, err))

	release := openrails.ReleaseOperationAuthorizationRequest{OperationID: a.OperationID, ReleaseReference: "absent:" + uuid.NewString()}
	auth, err = c.ReleaseOperationAuthorization(ctx, release)
	add(describeAuthorization("release", auth, err))
	auth, err = c.ReleaseOperationAuthorization(ctx, release)
	add(describeAuthorization("release replay", auth, err))
	_, err = c.ReleaseOperationAuthorization(ctx, openrails.ReleaseOperationAuthorizationRequest{OperationID: a.OperationID, ReleaseReference: "other"})
	add(describeAuthorization("release changed reference", nil, err))
	_, err = c.RecordProviderBillingObservation(ctx, f.observation(a.OperationID, "after-release", 1))
	add(describeQualification("observe released", nil, err))
	_, err = c.GetProviderBillingQualification(ctx, a.OperationID)
	add(describeQualification("qualification without evidence", nil, err))
	add("account after release: " + f.account(t, ctx))

	// One observation is never enough to settle, and release is fenced by it.
	b := f.authorization("settle", 1_000)
	auth, err = c.OpenOperationAuthorization(ctx, b)
	add(describeAuthorization("open billed", auth, err))
	first := f.observation(b.OperationID, "observation-1", 700, 800)
	q, err := c.RecordProviderBillingObservation(ctx, first)
	add(describeQualification("first observation", q, err))
	add("account after first observation: " + f.account(t, ctx))
	_, err = c.ReleaseOperationAuthorization(ctx, openrails.ReleaseOperationAuthorizationRequest{OperationID: b.OperationID, ReleaseReference: "absent"})
	add(describeAuthorization("release with evidence", nil, err))
	q, err = c.RecordProviderBillingObservation(ctx, first)
	add(describeQualification("first observation replay", q, err))
	changedEvidence := first
	changedEvidence.RawBody = []byte(`{"costs":"changed"}`)
	_, err = c.RecordProviderBillingObservation(ctx, changedEvidence)
	add(describeQualification("first observation changed", nil, err))
	premature := f.observation(b.OperationID, "observation-future", 700, 800)
	premature.QueryEnd = time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	_, err = c.RecordProviderBillingObservation(ctx, premature)
	add(describeQualification("query after observation time", nil, err))

	time.Sleep(providerBillingTestQuiescence + 100*time.Millisecond)
	second := first
	second.ObservationID = "observation-2"
	second.Records = []openrails.ProviderBillingRecord{first.Records[1], first.Records[0]}
	q, err = c.RecordProviderBillingObservation(ctx, second)
	add(describeQualification("second equal observation", q, err))
	add("account after settlement: " + f.account(t, ctx))
	q, err = c.RecordProviderBillingObservation(ctx, second)
	add(describeQualification("settling observation replay", q, err))
	q, err = c.GetProviderBillingQualification(ctx, b.OperationID)
	add(describeQualification("get qualification", q, err))
	auth, err = c.GetOperationAuthorization(ctx, b.OperationID)
	add(describeAuthorization("get settled", auth, err))
	add("account after replay: " + f.account(t, ctx))

	// Refused provider evidence keeps money reserved and cannot be superseded.
	d := f.authorization("refused", 1_000)
	_, err = c.OpenOperationAuthorization(ctx, d)
	require.NoError(t, err)
	refused := f.observation(d.OperationID, "refused-1")
	refused.Records = nil
	refused.RawBody = []byte(`{"amount":0.0000001}`)
	refused.Refusal = &openrails.ProviderBillingObservationRefusal{Kind: openrails.ProviderBillingRefusalSubmicroAmount}
	q, err = c.RecordProviderBillingObservation(ctx, refused)
	add(describeQualification("refused evidence", q, err))
	_, err = c.RecordProviderBillingObservation(ctx, f.observation(d.OperationID, "after-refusal", 1))
	add(describeQualification("observation after refusal", nil, err))
	add("account after refusal: " + f.account(t, ctx))

	oversized := f.observation(d.OperationID, "oversized")
	oversized.RawBody = bytes.Repeat([]byte("x"), openrails.ProviderBillingObservationMaxBytes*3/4)
	_, err = c.RecordProviderBillingObservation(ctx, oversized)
	add(describeQualification("oversized observation", nil, err))
	return out
}

func TestProviderObligationClientIsIdenticalEmbeddedAndStandalone(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD", integrationharness.WithConfig(withProviderBillingQuiescence))
	runtime := newProviderObligationRuntime(t, ctx, h)
	local, err := runtime.Client()
	require.NoError(t, err)

	transcripts := map[string][]string{}
	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": remote.Client()} {
		t.Run(name, func(t *testing.T) {
			transcripts[name] = providerObligationWorkflow(t, ctx, newProviderFixture(t, ctx, h, client, 10_000))
		})
	}
	require.Equal(t, transcripts["embedded"], transcripts["standalone"], "the shared Client must behave identically in both deployments")
	transcript := transcripts["standalone"]
	require.Equal(t, []string{
		"open: state=open replayed=false authorized=6000 terminal=false",
		"open replay: state=open replayed=true authorized=6000 terminal=false",
		"open changed body: status=409 code=operation_authorization_conflict param=authorization_body invalid=false not_found=false conflict=true insufficient=false",
		"open digest mismatch: status=400 code=invalid_param param= invalid=true not_found=false conflict=false insufficient=false",
		"open over capacity: status=402 code=insufficient_credits param= invalid=false not_found=false conflict=false insufficient=true",
		"account after open: balance=10000 held=6000 available=4000 owed=0",
		"get: state=open replayed=false authorized=6000 terminal=false",
		"get missing: status=404 code=operation_authorization_not_found param= invalid=false not_found=true conflict=false insufficient=false",
		"get dot segment: local invalid=true not_found=false conflict=false insufficient=false",
		"release: state=released replayed=false authorized=6000 terminal=true",
		"release replay: state=released replayed=true authorized=6000 terminal=true",
		"release changed reference: status=409 code=operation_authorization_conflict param=release_reference invalid=false not_found=false conflict=true insufficient=false",
		"observe released: status=409 code=operation_authorization_not_open param= invalid=false not_found=false conflict=true insufficient=false",
		"qualification without evidence: status=404 code=provider_billing_qualification_not_found param= invalid=false not_found=true conflict=false insufficient=false",
		"account after release: balance=10000 held=0 available=10000 owed=0",
	}, transcript[:15])
	require.Contains(t, transcript, "first observation: state=pending reason=awaiting_equal_observation replayed=false qualified_cost=null quiescence=1 | authorization: state=open replayed=false authorized=1000 terminal=false")
	require.Contains(t, transcript, "account after first observation: balance=10000 held=1000 available=9000 owed=0")
	require.Contains(t, transcript, "release with evidence: status=409 code=operation_authorization_has_billing_evidence param= invalid=false not_found=false conflict=true insufficient=false")
	require.Contains(t, transcript, "first observation changed: status=409 code=provider_billing_observation_conflict param=raw_body invalid=false not_found=false conflict=true insufficient=false")
	require.Contains(t, transcript, "second equal observation: state=eligible reason=eligible replayed=false qualified_cost=1500 quiescence=1 | authorization: state=settled replayed=false authorized=1000 terminal=true cost=1500 rated=1500 body_digest_ok=true")
	require.Contains(t, transcript, "account after settlement: balance=8500 held=0 available=8500 owed=0")
	require.Contains(t, transcript, "account after replay: balance=8500 held=0 available=8500 owed=0")
	require.Contains(t, transcript, "refused evidence: state=refused reason=provider_evidence_refused replayed=false qualified_cost=null quiescence=1 | authorization: state=open replayed=false authorized=1000 terminal=false")
	require.Contains(t, transcript, "observation after refusal: status=409 code=provider_billing_qualification_refused param= invalid=false not_found=false conflict=true insufficient=false")
	require.Contains(t, transcript, "account after refusal: balance=8500 held=1000 available=7500 owed=0")
	require.Contains(t, transcript, "oversized observation: status=400 code=invalid_param param= invalid=true not_found=false conflict=false insufficient=false")
}

// A caller cannot smuggle a rated or settlement amount through the wire: the
// strict decoder refuses it before any command runs.
func TestProviderObligationRoutesRefuseCallerRatedAmounts(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	for _, surface := range []*integrationharness.Surface{h.StartStandalone("USD"), h.StartEmbeddedHost("USD")} {
		t.Run(surface.Name, func(t *testing.T) {
			f := newProviderFixture(t, ctx, h, surface.Client(), 10_000)
			a := f.authorization("rated", 1_000)
			_, err := f.client.OpenOperationAuthorization(ctx, a)
			require.NoError(t, err)
			observation, err := json.Marshal(f.observation(a.OperationID, "rated-1", 1_000))
			require.NoError(t, err)

			for field, value := range map[string]string{
				"settlement_rated_usd_micros":         `"1"`,
				"settlement_provider_cost_usd_micros": `"1"`,
				"qualified_provider_cost_usd_micros":  `"1"`,
				"state":                               `"eligible"`,
			} {
				smuggled := []byte(string(bytes.TrimSuffix(observation, []byte("}"))) + `,"` + field + `":` + value + `}`)
				request, err := http.NewRequestWithContext(ctx, http.MethodPost,
					surface.BaseURL+"/v1/merchant/provider-operations/"+url.PathEscape(a.OperationID)+"/observations", bytes.NewReader(smuggled))
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer "+surface.Token)
				request.Header.Set("Content-Type", "application/json")
				response, err := http.DefaultClient.Do(request)
				require.NoError(t, err)
				body, _ := io.ReadAll(response.Body)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusBadRequest, response.StatusCode, "%s: %s", field, body)
				require.Contains(t, string(body), field)
			}

			_, err = f.client.GetProviderBillingQualification(ctx, a.OperationID)
			require.ErrorIs(t, err, openrails.ErrProviderBillingQualificationNotFound, "a refused request records no evidence")
			auth, err := f.client.GetOperationAuthorization(ctx, a.OperationID)
			require.NoError(t, err)
			require.Equal(t, openrails.OperationAuthorizationOpen, auth.State)
			require.Nil(t, auth.SettlementRatedUSDMicros)
			require.Equal(t, "balance=10000 held=1000 available=9000 owed=0", f.account(t, ctx))
		})
	}
}
