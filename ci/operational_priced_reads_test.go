//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// Inspection and replay must read the stored charge, including integer
// quantities beyond float64's exact range, after the catalog price changes.
func TestOperationalReadsPreservePricedQuantities(t *testing.T) {
	f := newFixture(t)
	engine, embedded := f.runtimeWithStripe(t, "quantity-inspection", nil)
	product, err := embedded.CreateProduct(t.Context(), billing.CreateProductParams{Key: "usage", DisplayName: "Usage"})
	require.NoError(t, err)
	p := &pricing{t: t, ctx: t.Context(), client: embedded, product: product.ID, started: time.Now().UTC().Truncate(time.Second)}
	setPrice := func(amount int64) {
		p.meter("tokens", "large", catalog.AggregationSum, "tokens", map[string]string{"model": "metadata.model"},
			catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{DivideBy: 1 << 53,
				Matrix: &catalog.Matrix{Dimension: "model", Cells: map[string]catalog.MatrixCell{"small": {UnitAmount: amount}}}}}, nil)
	}
	setPrice(1)

	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, engine, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))
	server := httptest.NewServer(mux)
	defer server.Close()
	scope := authtest.MerchantScope(embedded.MerchantID())
	remote := func(permissions ...billing.Permission) *openrails.Client {
		key := auth.Issue(authtest.Grant{Identity: authtest.Application("inspection"), Scope: scope, Bound: scope, Permissions: permissions})
		client, err := openrails.NewRemoteMerchant(server.URL, openrails.RemoteCredential{APIKey: key})
		require.NoError(t, err)
		return client
	}
	reader := remote(billing.MerchantUsageRead)
	writers := []struct {
		name   string
		client *openrails.Client
	}{{"embedded", embedded}, {"remote", remote(billing.MerchantUsageManage)}}
	const quantity int64 = 1<<53 + 1
	cell := "small"
	wantCharges := []billing.UsageCharge{{MeterKey: "tokens", Cell: &cell, Quantity: quantity, Amount: 1}}
	type operation struct {
		client  *openrails.Client
		admit   billing.AdmitParams
		capture billing.CaptureAdmissionParams
		receipt *billing.CaptureReceipt
		event   *billing.UsageEvent
		key     openrails.RequestOption
		balance int64
	}
	var operations []operation
	for _, writer := range writers {
		customer := billing.CustomerID(uuid.New())
		p.grant(customer, 1_000_000)
		dimensions := map[string]int64{"tokens": quantity}
		metadata := map[string]any{"model": cell}
		events, err := writer.client.RecordUsage(t.Context(), []billing.RecordUsageParams{{CustomerID: customer, Invoker: customer.String(), Currency: "USD",
			EventType: "large", Source: "inspection", SourceID: writer.name, Dimensions: dimensions, Metadata: metadata}})
		require.NoError(t, err)
		require.NoError(t, events[0].Err())
		require.Equal(t, wantCharges, events[0].Event.Charges)
		deadline := p.started.Add(time.Hour)
		admit := billing.AdmitParams{RequestID: writer.name, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
			Currency: "USD", Usage: &billing.UsageEstimate{EventType: "large", Dimensions: dimensions, Metadata: metadata}, ExpiresAt: &deadline}
		key := openrails.WithIdempotencyKey(uuid.NewString())
		verdicts, err := writer.client.Admit(t.Context(), []billing.AdmitParams{admit}, key)
		require.NoError(t, err)
		require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
		require.EqualValues(t, 1, verdicts[0].Admission.EstimatedAmount)
		capture := billing.CaptureAdmissionParams{RequestID: admit.RequestID, Usage: &billing.CaptureUsage{EventType: "large", Dimensions: dimensions, Metadata: metadata}}
		captures, err := writer.client.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture}, key)
		require.NoError(t, err)
		require.NoError(t, captures[0].Err())
		require.Equal(t, wantCharges, captures[0].Receipt.Charges)
		require.NotNil(t, captures[0].Receipt.UsageEventID)
		operations = append(operations, operation{writer.client, admit, capture, captures[0].Receipt, events[0].Event, key, balanceOf(t, embedded, customer)})
	}

	setPrice(9)
	for _, op := range operations {
		t.Run(op.admit.RequestID, func(t *testing.T) {
			for _, client := range []*openrails.Client{embedded, reader} {
				for _, id := range []billing.UsageEventID{op.event.ID, *op.receipt.UsageEventID} {
					event, err := client.GetUsageEvent(t.Context(), id)
					require.NoError(t, err)
					require.Equal(t, quantity, event.Dimensions["tokens"])
					require.Equal(t, wantCharges, event.Charges)
					require.EqualValues(t, 1, event.Amount)
					require.False(t, event.Replayed)
					page, err := client.ListUsageEvents(t.Context(), billing.UsageEventListParams{CustomerID: op.admit.CustomerID, Source: event.Source, SourceID: event.SourceID})
					require.NoError(t, err)
					require.Len(t, page.Items, 1)
					require.Equal(t, *event, page.Items[0], "list and lookup read the same stored charge")
				}
			}
			verdicts, err := op.client.Admit(t.Context(), []billing.AdmitParams{op.admit}, op.key)
			require.NoError(t, err)
			require.True(t, verdicts[0].Admission.Replayed)
			require.Equal(t, billing.AdmissionCaptured, *verdicts[0].Admission.State)
			require.False(t, verdicts[0].Allowed(), "the original HTTP key cannot reopen a captured hold")
			require.EqualValues(t, 1, verdicts[0].Admission.EstimatedAmount)
			captures, err := op.client.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{op.capture}, op.key)
			require.NoError(t, err)
			require.NoError(t, captures[0].Err())
			want := *op.receipt
			want.Replayed = true
			require.Equal(t, &want, captures[0].Receipt, "replay keeps the first receipt and ledger identity")

			// The adjacent quantity rounds to the same charge but changes the
			// immutable terms of both the estimate and the capture.
			changedAdmit := op.admit
			changedAdmit.Usage = &billing.UsageEstimate{EventType: "large", Dimensions: map[string]int64{"tokens": quantity - 1}, Metadata: op.admit.Usage.Metadata}
			verdicts, err = op.client.Admit(t.Context(), []billing.AdmitParams{changedAdmit}, op.key)
			require.NoError(t, err)
			require.NotNil(t, verdicts[0].Error)
			require.Equal(t, billing.CodeIdempotencyKeyReused, verdicts[0].Error.Code)
			changedCapture := op.capture
			changedCapture.Usage = &billing.CaptureUsage{EventType: "large", Dimensions: map[string]int64{"tokens": quantity - 1}, Metadata: op.capture.Usage.Metadata}
			captures, err = op.client.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{changedCapture}, op.key)
			require.NoError(t, err)
			require.ErrorIs(t, captures[0].Err(), billing.ErrIdempotencyKeyReused)
			require.Equal(t, op.balance, balanceOf(t, embedded, op.admit.CustomerID), "inspection and replay leave the ledger unchanged")
		})
	}
}
