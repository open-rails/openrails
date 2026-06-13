package sigverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func stripeHeader(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func nmiHeader(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,s=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyStripe(t *testing.T) {
	secret, body := "whsec", []byte(`{"id":"evt"}`)
	now := fmt.Sprintf("%d", time.Now().Unix())

	if err := VerifyStripe(secret, stripeHeader(secret, now, body), body, time.Minute); err != nil {
		t.Fatalf("fresh valid signature: %v", err)
	}
	if err := VerifyStripe(secret, stripeHeader(secret, now, body), []byte(`{"id":"x"}`), time.Minute); err == nil {
		t.Fatal("tampered body accepted")
	}
	// Old timestamp rejected when a window is enforced...
	old := stripeHeader(secret, "1700000000", body)
	if err := VerifyStripe(secret, old, body, time.Minute); err == nil {
		t.Fatal("stale timestamp accepted within window")
	}
	// ...but accepted when tolerance<=0 (queued re-verify path).
	if err := VerifyStripe(secret, old, body, 0); err != nil {
		t.Fatalf("tolerance=0 should skip the window: %v", err)
	}
}

func TestVerifyNMI(t *testing.T) {
	secret, body := "nmi", []byte(`{"event_id":"n"}`)
	now := fmt.Sprintf("%d", time.Now().Unix())

	if err := VerifyNMI(secret, nmiHeader(secret, now, body), body, 5*time.Minute); err != nil {
		t.Fatalf("fresh valid signature: %v", err)
	}
	if err := VerifyNMI(secret, nmiHeader(secret, now, body), []byte("x"), 5*time.Minute); err == nil {
		t.Fatal("tampered body accepted")
	}
	old := nmiHeader(secret, "1700000000", body)
	if err := VerifyNMI(secret, old, body, 5*time.Minute); err == nil {
		t.Fatal("stale timestamp accepted within window")
	}
	if err := VerifyNMI(secret, old, body, 0); err != nil {
		t.Fatalf("tolerance=0 should skip the window: %v", err)
	}
}

func TestParsersRejectGarbage(t *testing.T) {
	if ts, sigs := ParseStripeSignatureHeader("garbage"); ts != "" || len(sigs) != 0 {
		t.Fatalf("expected empty parse, got ts=%q sigs=%v", ts, sigs)
	}
	if _, _, err := ParseNMISignatureHeader("garbage"); err == nil {
		t.Fatal("expected error for malformed NMI header")
	}
}
