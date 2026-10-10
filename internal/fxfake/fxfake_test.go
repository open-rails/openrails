package fxfake

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The fake answers through Transport; Guard refuses and records a request
// for the real exchange-api, and lets every other through.
func TestTransportAndGuard(t *testing.T) {
	fx := New()
	defer fx.Close()
	escaped := Guard()

	resp, err := (&http.Client{Transport: fx.Transport()}).Get("https://" + Primary + "/v1/currencies/eur.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `"eur":{`) || fx.Requests(Primary, "eur") != 1 {
		t.Fatalf("fake answered %q, counted %d", body, fx.Requests(Primary, "eur"))
	}

	_, err = http.Get("https://" + Fallback + "/npm/@fawazahmed0/currency-api@latest/v1/currencies/usd.json")
	if !errors.Is(err, ErrEscaped) {
		t.Fatalf("an escaped request must be refused, got %v", err)
	}
	if got := escaped(); len(got) != 1 || !strings.Contains(got[0], Fallback) {
		t.Fatalf("escaped = %v", got)
	}
}
