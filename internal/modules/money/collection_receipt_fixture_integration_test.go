//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

type receiptFixtureNMI struct {
	client  *nmi.NMIClient
	request money.ChargeRequest
}

func (r receiptFixtureNMI) ResolveNMIClient(_ context.Context, merchant uuid.UUID, psp *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if merchant != r.request.MerchantID || psp == nil || *psp != r.request.Instrument.PSPID {
		return nil, false, fmt.Errorf("fixture account mismatch")
	}
	return r.client, true, nil
}

// readChargedRequest exposes the request actually received by a scripted
// provider as HTTP facts. It cannot derive a successful payment from current
// operation terms: replay must match an earlier submitted request.
func readChargedRequest(ctx context.Context, in gen.OpenrailsRailIntent, request money.ChargeRequest, txn, reference string) (intents.CollectedReceipt, bool, error) {
	invoiceID := "in_" + request.IdempotencyKey
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			var body any
			switch r.URL.Path {
			case "/v1/invoices/" + invoiceID:
				body = map[string]any{"id": invoiceID, "status": "paid", "amount_paid": int64(request.AmountCents), "currency": request.Currency, "customer": request.ProviderCustomerRef, "charge": txn, "metadata": map[string]string{subscriptions.StripeCollectionKeyMetadata: request.IdempotencyKey}}
			case "/v1/charges/" + txn:
				body = map[string]any{"id": txn, "invoice": invoiceID, "amount_captured": int64(request.AmountCents), "currency": request.Currency, "customer": request.ProviderCustomerRef, "payment_method": request.Instrument.RailMethodRef, "paid": true, "captured": true, "status": "succeeded"}
			default:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/payments/") {
			amount := fmt.Sprintf("%d.%02d", request.AmountCents/100, request.AmountCents%100)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": txn, "object": "transaction", "response": "1", "amount": amount, "currency": request.Currency, "customer_vault_id": request.Instrument.RailCustomerRef, "actions": []map[string]any{{"type": "sale", "amount": amount, "success": true}}})
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("order_id") != request.IdempotencyKey {
			fmt.Fprint(w, "<nm_response/>")
			return
		}
		fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, txn, request.IdempotencyKey)
	}))
	defer server.Close()
	if in.Rail == "stripe" {
		if reference == "" || reference == txn {
			reference = invoiceID
		}
		service := subscriptions.NewAccountStripeService(nil, request.MerchantID, request.Instrument.PSPID, "fixture", "sk_test_fixture")
		service.SetBaseURLForTest(server.URL)
		return intents.ReadStripeCollectionReceipt(ctx, in, service, reference)
	}
	client, err := nmi.NewAccountClient(request.MerchantID, request.Instrument.PSPID, "nmi", &config.NMIProviderSettings{SecurityKey: "synthetic-key"}, true)
	if err != nil {
		return intents.CollectedReceipt{}, false, err
	}
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL
	return intents.ReadNMICollectionReceipt(ctx, in, receiptFixtureNMI{client, request}, reference)
}

func (f *fakeCharger) ReadCollectionReceipt(ctx context.Context, in gen.OpenrailsRailIntent, reference string) (intents.CollectedReceipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	txn, ok := f.landed[in.ID.String()]
	if !ok {
		return intents.CollectedReceipt{}, false, nil
	}
	for _, request := range f.charges {
		if request.IdempotencyKey == in.ID.String() {
			return readChargedRequest(ctx, in, request, txn, reference)
		}
	}
	return intents.CollectedReceipt{}, false, fmt.Errorf("provider candidate has no original submitted request")
}

func (h *hookCharger) ReadCollectionReceipt(ctx context.Context, in gen.OpenrailsRailIntent, reference string) (intents.CollectedReceipt, bool, error) {
	for _, request := range h.charges {
		if request.IdempotencyKey == in.ID.String() {
			return readChargedRequest(ctx, in, request, "tx_"+request.IdempotencyKey, reference)
		}
	}
	return intents.CollectedReceipt{}, false, nil
}

func (h *hookCharger) ConfirmCollectionNotExecuted(context.Context, gen.OpenrailsRailIntent) error {
	return fmt.Errorf("fixture does not attest nonexecution")
}

func (f *fakeCollectionAdapter) ReadCollectionReceipt(ctx context.Context, in gen.OpenrailsRailIntent, reference string) (intents.CollectedReceipt, bool, error) {
	if f.decline {
		return intents.CollectedReceipt{}, false, nil
	}
	for _, request := range f.charges {
		if request.IdempotencyKey == in.ID.String() {
			return readChargedRequest(ctx, in, request, "tx_"+request.IdempotencyKey, reference)
		}
	}
	return intents.CollectedReceipt{}, false, nil
}

func (f *fakeCollectionAdapter) ConfirmCollectionNotExecuted(context.Context, gen.OpenrailsRailIntent) error {
	return fmt.Errorf("fixture does not attest nonexecution")
}

// standaloneCollectionReader exercises a test's actual loopback provider with
// the same account-scoped clients its isolated adapter uses.
type standaloneCollectionReader struct {
	nmi    intents.NMIClientResolver
	stripe *subscriptions.StripeService
}

func (r standaloneCollectionReader) ReadCollectionReceipt(ctx context.Context, in gen.OpenrailsRailIntent, reference string) (intents.CollectedReceipt, bool, error) {
	if r.stripe != nil {
		return intents.ReadStripeCollectionReceipt(ctx, in, r.stripe, reference)
	}
	return intents.ReadNMICollectionReceipt(ctx, in, r.nmi, reference)
}

func (r standaloneCollectionReader) ConfirmCollectionNotExecuted(context.Context, gen.OpenrailsRailIntent) error {
	return fmt.Errorf("fixture does not attest nonexecution")
}
