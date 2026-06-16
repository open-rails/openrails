package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/shared/webhookutil"
)

func stripeSig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// verifyStripeMerchant is the trust boundary for the merchant webhook surface: it
// verifies the inbound signature against the RESOLVED TENANT's signing secrets
// (loaded after tenant resolution). This asserts: the matching tenant secret
// verifies; a wrong/foreign tenant's secret is rejected; and a missing signature
// is rejected (a router-supplied tenant is never trusted on its own).
func TestVerifyStripeTenant_TrustBoundary(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())

	merchantSecret := "whsec_tenant_acme"
	otherSecret := "whsec_other_tenant"
	header := stripeSig(merchantSecret, ts, body)

	// The resolved tenant's secret verifies the signature.
	prepared, err := verifyStripeMerchant(body, []string{merchantSecret}, header)
	if err != nil {
		t.Fatalf("expected verification with tenant secret, got %v", err)
	}
	if prepared.EventID != "evt_1" {
		t.Fatalf("event id = %q, want evt_1", prepared.EventID)
	}

	// A DIFFERENT tenant's secret must NOT verify this signature (no cross-tenant).
	if _, err := verifyStripeMerchant(body, []string{otherSecret}, header); err == nil {
		t.Fatal("foreign tenant secret must not verify the signature")
	}

	// Multi-secret (snapshot + thin): first non-matching, second matching.
	if _, err := verifyStripeMerchant(body, []string{otherSecret, merchantSecret}, header); err != nil {
		t.Fatalf("multi-secret verify should accept matching secret, got %v", err)
	}

	// A missing signature header is rejected — the router is not the trust boundary.
	if _, err := verifyStripeMerchant(body, []string{merchantSecret}, ""); err == nil {
		t.Fatal("missing signature must be rejected")
	} else if err != webhookutil.ErrWebhookSignatureMissing {
		t.Fatalf("missing signature err = %v, want ErrWebhookSignatureMissing", err)
	}
}
