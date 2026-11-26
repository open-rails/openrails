package nmi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSaleRefusalClassification(t *testing.T) {
	for _, code := range []int{300, 400, 410, 411, 420, 421, 430, 440, 441, 460, 461, 999} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			_, err := ParseSaleResponse(fmt.Sprintf("response=3&response_code=%d&responsetext=Refused&customer_vault_id=vault", code), 100, "USD")
			refused := code == 300 || code == 410 || code == 411 || code == 440 || code == 441 || code == 460 || code == 461
			if errors.Is(err, ErrSaleRefused) != refused || !RequiresVerification(err) {
				t.Fatalf("classification: %v", err)
			}
			if refused {
				var detail *CustomerVaultError
				if !errors.As(err, &detail) || detail.ResponseCode != code || detail.LocalizationID == "" || detail.CustomerVaultID != "vault" {
					t.Fatalf("refusal details: %+v", detail)
				}
			}
		})
	}
	fields := url.Values{"response": {"3"}, "response_code": {"300"}}
	_, verificationErr := ParseSaleResponse(fields.Encode(), 0, "USD")
	for _, err := range []error{newSaleError(fields.Encode(), fields, false), verificationErr} {
		var refusal *CustomerVaultError
		if !RequiresVerification(err) || errors.Is(err, ErrSaleRefused) || errors.As(err, &refusal) {
			t.Fatalf("non-sale verification changed: %v", err)
		}
	}
	for _, raw := range []string{"", "garbage", "response=3&response_code=no", "response=4&response_code=300"} {
		_, err := ParseSaleResponse(raw, 100, "USD")
		if errors.Is(err, ErrSaleRefused) || !RequiresVerification(err) {
			t.Fatalf("unreadable answer %q: %v", raw, err)
		}
	}
	_, err := ParseSaleResponse("response=3&response_code=300&responsetext=Duplicate+transaction", 100, "USD")
	if !errors.Is(err, ErrDuplicateTransaction) || errors.Is(err, ErrSaleRefused) || !RequiresVerification(err) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestDirectDispatchBoundary(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("closed server received request") }))
	closed.Close()
	lost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer lost.Close()
	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer failure.Close()
	tlsFailure := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS server received request") }))
	defer tlsFailure.Close()
	for _, tc := range []struct {
		name, url string
		unsent    bool
	}{{"connection-refused", closed.URL, true}, {"TLS-dial-failure", tlsFailure.URL, true}, {"lost-answer", lost.URL, false}, {"empty-5xx", failure.URL, false}} {
		t.Run(tc.name, func(t *testing.T) {
			client := &NMIClient{Log: slog.Default(), DirectPostURL: tc.url, LoopbackFixture: true}
			_, err := client.sendDirectBody(context.Background(), "sale", []byte("type=sale"))
			if err == nil || errors.Is(err, ErrNotDispatched) != tc.unsent || RequiresVerification(err) == tc.unsent {
				t.Fatalf("dispatch classification: %v", err)
			}
		})
	}
}
