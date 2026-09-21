package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

type engineWire func(*http.Request) (*http.Response, error)

func (f engineWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func engineResponse(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(b))), Header: make(http.Header)}
}
func engineFixture() (*StripeService, StripeEnginePaymentParams) {
	p := StripeEnginePaymentParams{MerchantID: uuid.New(), PSPID: uuid.New(), CustomerID: uuid.New(), OperationID: uuid.New(), AmountMinor: 1299, Currency: "USD", Initial: true}
	p.Instrument = charge.FrozenInstrument{PSPID: p.PSPID, Custodian: models.CustodianPSP, RailCustomerRef: "cus_fixture", RailMethodRef: "pm_fixture"}
	return NewAccountStripeService(&config.Config{ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox}, p.MerchantID, p.PSPID, "acct_fixture", "sk_test_fixture"), p
}
func enginePI(p StripeEnginePaymentParams) map[string]any {
	return map[string]any{"id": "pi_fixture", "status": "succeeded", "customer": p.Instrument.RailCustomerRef, "payment_method": p.Instrument.RailMethodRef, "amount": 1299, "amount_received": 1299, "currency": "usd", "setup_future_usage": "off_session", "capture_method": "automatic", "confirmation_method": "automatic", "latest_charge": "ch_fixture", "metadata": p.metadata(), "livemode": false}
}
func engineCharge(p StripeEnginePaymentParams) map[string]any {
	return map[string]any{"id": "ch_fixture", "amount": 1299, "amount_captured": 1299, "currency": "usd", "customer": p.Instrument.RailCustomerRef, "payment_method": p.Instrument.RailMethodRef, "payment_intent": "pi_fixture", "status": "succeeded", "paid": true, "captured": true}
}
func installEngineWire(t *testing.T, f engineWire) {
	t.Helper()
	release := stripeapi.InstallBaseTransport(f)
	t.Cleanup(release)
}

func TestStripeEngineCreateAndReadSamePayment(t *testing.T) {
	s, p := engineFixture()
	var posts int
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get(stripeapi.VersionHeader) != stripeapi.APIVersion || r.Header.Get("Authorization") != "Bearer sk_test_fixture" {
			t.Fatalf("unguarded or wrong scoped credentials")
		}
		if r.Method == "POST" {
			posts++
			if r.URL.Path != "/v1/payment_intents" {
				t.Fatalf("unexpected billing object %s", r.URL.Path)
			}
			if r.Header.Get(stripeapi.IdempotencyKeyHeader) != "engine:"+p.OperationID.String() {
				t.Fatal("missing accepted operation key")
			}
			b, _ := io.ReadAll(r.Body)
			v, _ := url.ParseQuery(string(b))
			for k, want := range map[string]string{"customer": "cus_fixture", "payment_method": "pm_fixture", "amount": "1299", "currency": "usd", "confirm": "true", "capture_method": "automatic", "off_session": "false", "setup_future_usage": "off_session", "payment_method_types[]": "card"} {
				if v.Get(k) != want {
					t.Fatalf("%s=%q", k, v.Get(k))
				}
			}
			if v.Has("price") || v.Has("subscription") {
				t.Fatal("native billing enrollment")
			}
		}
		if r.URL.Path == "/v1/charges/ch_fixture" {
			return engineResponse(200, engineCharge(p)), nil
		}
		return engineResponse(200, enginePI(p)), nil
	})
	result, err := s.CreateEnginePayment(context.Background(), p)
	if err != nil || result.State != StripeEngineSucceeded || result.Receipt == nil {
		t.Fatalf("%+v %v", result, err)
	}
	if err := result.Receipt.Matches(p); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r, found, err := s.ReadEnginePayment(context.Background(), p, result.PaymentIntentID)
		if err != nil || !found || r.Receipt.ChargeID != result.Receipt.ChargeID {
			t.Fatalf("%+v %v", r, err)
		}
	}
	if posts != 1 {
		t.Fatalf("replay moved money %d times", posts)
	}
}
func TestStripeEngineRenewalOffSession(t *testing.T) {
	s, p := engineFixture()
	p.Initial = false
	p.Instrument.StoredCredentialRecurringRef = "pi_initial"
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			v, _ := url.ParseQuery(string(b))
			if v.Get("off_session") != "true" || v.Has("setup_future_usage") || v.Get("metadata[openrails_agreement]") != "pi_initial" {
				t.Fatalf("wrong recurring flags: %s", b)
			}
		}
		if r.URL.Path == "/v1/charges/ch_fixture" {
			return engineResponse(200, engineCharge(p)), nil
		}
		return engineResponse(200, enginePI(p)), nil
	})
	r, err := s.CreateEnginePayment(context.Background(), p)
	if err != nil || r.State != StripeEngineSucceeded {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestStripeEngineLostResponseRecoveryNeverPosts(t *testing.T) {
	s, p := engineFixture()
	posts := 0
	lists := 0
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			posts++
			return nil, errors.New("simulated timeout after provider accepted")
		}
		if r.URL.Path == "/v1/payment_intents" {
			lists++
			if r.URL.Query().Get("customer") != "cus_fixture" {
				t.Fatal("unscoped list")
			}
			if r.URL.Query().Get("starting_after") == "" {
				return engineResponse(200, map[string]any{"data": []any{map[string]any{"id": "pi_other", "metadata": map[string]string{}}}, "has_more": true}), nil
			}
			return engineResponse(200, map[string]any{"data": []any{enginePI(p)}, "has_more": false}), nil
		}
		if r.URL.Path == "/v1/charges/ch_fixture" {
			return engineResponse(200, engineCharge(p)), nil
		}
		return engineResponse(200, enginePI(p)), nil
	})
	if _, err := s.CreateEnginePayment(context.Background(), p); err == nil {
		t.Fatal("timeout succeeded")
	}
	r, found, err := s.ReadEnginePayment(context.Background(), p, "")
	if err != nil || !found || r.State != StripeEngineSucceeded || posts != 1 || lists != 2 {
		t.Fatalf("%+v found=%v err=%v posts=%d lists=%d", r, found, err, posts, lists)
	}
}
func TestStripeEngineAuthenticationKeepsPaymentIdentityAndSecretPrivate(t *testing.T) {
	s, p := engineFixture()
	pi := enginePI(p)
	pi["status"] = "requires_payment_method"
	pi["payment_method"] = nil
	pi["last_payment_error"] = map[string]any{"code": "authentication_required", "payment_method": map[string]any{"id": "pm_fixture"}}
	pi["client_secret"] = "pi_fixture_secret_sensitive"
	posts := 0
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			posts++
			return engineResponse(402, map[string]any{"error": map[string]any{"payment_intent": pi}}), nil
		}
		return engineResponse(200, pi), nil
	})
	r, err := s.CreateEnginePayment(context.Background(), p)
	if err != nil || r.State != StripeEngineAuthenticationRequired || r.PaymentIntentID != "pi_fixture" {
		t.Fatalf("%+v %v", r, err)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("secret persisted in outcome")
	}
	secret, err := s.EngineAuthenticationSecret(context.Background(), p, r.PaymentIntentID, p.CustomerID)
	if err != nil || secret != "pi_fixture_secret_sensitive" {
		t.Fatalf("recovery secret unavailable: %v", err)
	}
	if secret, err := s.EngineAuthenticationSecret(context.Background(), p, r.PaymentIntentID, uuid.New()); err == nil || secret != "" {
		t.Fatal("cross-customer recovery authorized")
	}
	if posts != 1 {
		t.Fatal("authentication recovery creates second charge")
	}
}
func TestStripeEngineRejectsWrongEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{"operation", func(pi, ch map[string]any) {
			pi["metadata"].(map[string]string)["openrails_engine_operation"] = uuid.NewString()
		}},
		{"merchant", func(pi, ch map[string]any) {
			pi["metadata"].(map[string]string)["openrails_merchant"] = uuid.NewString()
		}},
		{"psp", func(pi, ch map[string]any) { pi["metadata"].(map[string]string)["openrails_psp"] = uuid.NewString() }},
		{"amount", func(pi, ch map[string]any) { pi["amount"] = 1300 }},
		{"received", func(pi, ch map[string]any) { pi["amount_received"] = 1 }},
		{"currency", func(pi, ch map[string]any) { pi["currency"] = "eur" }},
		{"customer", func(pi, ch map[string]any) { pi["customer"] = "cus_other" }},
		{"method", func(pi, ch map[string]any) { pi["payment_method"] = "pm_replacement" }},
		{"setup", func(pi, ch map[string]any) { pi["setup_future_usage"] = "on_session" }},
		{"live", func(pi, ch map[string]any) { pi["livemode"] = true }},
		{"missing_environment", func(pi, ch map[string]any) { delete(pi, "livemode") }},
		{"charge_customer", func(pi, ch map[string]any) { ch["customer"] = "cus_other" }},
		{"charge_method", func(pi, ch map[string]any) { ch["payment_method"] = "pm_other" }},
		{"charge_pi", func(pi, ch map[string]any) { ch["payment_intent"] = "pi_other" }},
		{"charge_capture", func(pi, ch map[string]any) { ch["captured"] = false }},
		{"charge_amount", func(pi, ch map[string]any) { ch["amount_captured"] = 1 }},
		{"charge_refunded", func(pi, ch map[string]any) { ch["refunded"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := engineFixture()
			pi, ch := enginePI(p), engineCharge(p)
			tc.mutate(pi, ch)
			installEngineWire(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" {
					t.Fatal("read mutated provider")
				}
				if r.URL.Path == "/v1/charges/ch_fixture" {
					return engineResponse(200, ch), nil
				}
				return engineResponse(200, pi), nil
			})
			if _, _, err := s.ReadEnginePayment(context.Background(), p, "pi_fixture"); err == nil {
				t.Fatal("foreign/incomplete evidence accepted")
			}
		})
	}
}
func TestStripeEngineReadOnlyAndAccountRefuseBeforeWire(t *testing.T) {
	s, p := engineFixture()
	calls := 0
	installEngineWire(t, func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected wire") })
	s.Config.ProviderWriteMode = config.ProviderWriteModeReadOnly
	if _, err := s.CreateEnginePayment(context.Background(), p); !errors.Is(err, charge.ErrNotDispatched) {
		t.Fatalf("read-only not proven non-dispatched: %v", err)
	}
	p.PSPID = uuid.New()
	if _, err := s.CreateEnginePayment(context.Background(), p); !errors.Is(err, charge.ErrNotDispatched) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("unarmed charge reached wire")
	}
}
func TestStripeEngineRecoveryMissingOrDuplicateNeverResends(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(strconvBool(duplicate), func(t *testing.T) {
			s, p := engineFixture()
			installEngineWire(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" {
					t.Fatal("recovery write")
				}
				data := []any{}
				if duplicate {
					data = []any{enginePI(p), enginePI(p)}
				}
				return engineResponse(200, map[string]any{"data": data, "has_more": false}), nil
			})
			_, found, err := s.ReadEnginePayment(context.Background(), p, "")
			if duplicate && err == nil || !duplicate && (found || err != nil) {
				t.Fatalf("found=%v err=%v", found, err)
			}
		})
	}
}
func strconvBool(v bool) string {
	if v {
		return "duplicate"
	}
	return "absent"
}
func TestStripeEngineConcurrentRecoveryReads(t *testing.T) {
	s, p := engineFixture()
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Error("concurrent recovery attempted write")
		}
		if r.URL.Path == "/v1/charges/ch_fixture" {
			return engineResponse(200, engineCharge(p)), nil
		}
		return engineResponse(200, enginePI(p)), nil
	})
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, found, err := s.ReadEnginePayment(context.Background(), p, "pi_fixture")
			if err != nil || !found || r.State != StripeEngineSucceeded {
				t.Errorf("%+v %v", r, err)
			}
		}()
	}
	wg.Wait()
}

func TestStripeEngineReversalRetainsOriginalCapture(t *testing.T) {
	for _, kind := range []string{"refund", "dispute", "partial_refund"} {
		t.Run(kind, func(t *testing.T) {
			s, p := engineFixture()
			ch := engineCharge(p)
			if kind == "refund" {
				ch["refunded"] = true
				ch["amount_refunded"] = 1299
			} else if kind == "partial_refund" {
				ch["amount_refunded"] = 1
			} else {
				ch["disputed"] = true
			}
			installEngineWire(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v1/charges/ch_fixture" {
					return engineResponse(200, ch), nil
				}
				return engineResponse(200, enginePI(p)), nil
			})
			result, found, err := s.ReadEnginePayment(context.Background(), p, "pi_fixture")
			if err != nil || !found || result.Receipt == nil {
				t.Fatalf("%+v %v", result, err)
			}
			want := kind
			if kind == "partial_refund" {
				want = ""
			}
			if result.Receipt.ReversalKind() != want {
				t.Fatal("lost reversal facts")
			}
			if err := result.Receipt.Matches(p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStripeEngineCustomerRetryKeepsRecurringAgreement(t *testing.T) {
	s, p := engineFixture()
	p.Initial = false
	p.CustomerInitiated = true
	p.Instrument.StoredCredentialRecurringRef = "pi_original"
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			v, _ := url.ParseQuery(string(b))
			if v.Get("off_session") != "false" || v.Has("setup_future_usage") || v.Get("metadata[openrails_customer_retry]") != "true" || v.Get("metadata[openrails_agreement]") != "pi_original" {
				t.Fatalf("retry changed recurring agreement or initiation: %s", b)
			}
		}
		if r.URL.Path == "/v1/charges/ch_fixture" {
			return engineResponse(200, engineCharge(p)), nil
		}
		return engineResponse(200, enginePI(p)), nil
	})
	r, err := s.CreateEnginePayment(context.Background(), p)
	if err != nil || r.State != StripeEngineSucceeded || r.Receipt == nil {
		t.Fatalf("%+v %v", r, err)
	}
	if err := r.Receipt.Matches(p); err != nil {
		t.Fatal(err)
	}
	p.CustomerInitiated = false
	if err := r.Receipt.Matches(p); err == nil {
		t.Fatal("customer retry receipt accepted for a merchant instruction")
	}
}

func TestStripeEngineRetryUsesQualifiedSetupAnchor(t *testing.T) {
	s, p := engineFixture()
	p.Initial = false
	p.CustomerInitiated = true
	p.Instrument.StoredCredentialRecurringRef = "seti_replacement"
	installEngineWire(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			v, _ := url.ParseQuery(string(b))
			if v.Get("metadata[openrails_agreement]") != "seti_replacement" || v.Get("off_session") != "false" || v.Has("setup_future_usage") {
				t.Fatal("lost setup-bound customer retry")
			}
		}
		if r.URL.Path == "/v1/charges/ch_fixture" {
			return engineResponse(200, engineCharge(p)), nil
		}
		return engineResponse(200, enginePI(p)), nil
	})
	result, err := s.CreateEnginePayment(context.Background(), p)
	if err != nil || result.State != StripeEngineSucceeded {
		t.Fatalf("%+v %v", result, err)
	}
}
