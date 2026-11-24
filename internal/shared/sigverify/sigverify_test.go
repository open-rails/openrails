package sigverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func nmiHeader(secret, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(nonce + "."))
	mac.Write(body)
	return "t=" + nonce + ",s=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyNMIReadsTheNonceAsOpaque(t *testing.T) {
	body := []byte(`{"event_id":"evt_1"}`)
	for _, nonce := range []string{"a1b2c3d4e5f6", "1000000000", "x,s=y", "nonce with spaces"} {
		header := nmiHeader("secret", nonce, body)
		got, _, err := ParseNMISignatureHeader(header)
		if err != nil || got != nonce {
			t.Fatalf("nonce %q parsed as %q: %v", nonce, got, err)
		}
		if err := VerifyNMI("secret", header, body); err != nil {
			t.Fatalf("nonce %q: %v", nonce, err)
		}
	}
	upper := nmiHeader("secret", "n", body)
	cut := strings.LastIndex(upper, ",s=") + 3
	if err := VerifyNMI("secret", upper[:cut]+strings.ToUpper(upper[cut:]), body); err != nil {
		t.Fatalf("upper-case hex: %v", err)
	}
}

func TestVerifyNMIRefuses(t *testing.T) {
	body := []byte(`{"event_id":"evt_1"}`)
	good := nmiHeader("secret", "nonce", body)
	for name, header := range map[string]string{
		"other secret":  nmiHeader("other", "nonce", body),
		"other nonce":   strings.Replace(good, "t=nonce", "t=nonce2", 1),
		"no nonce":      strings.Replace(good, "t=nonce", "t=", 1),
		"no signature":  "t=nonce,s=",
		"no t":          strings.TrimPrefix(good, "t=nonce,"),
		"not hex":       "t=nonce,s=zz",
		"empty":         "",
		"stripe format": "t=1,v1=" + good[strings.LastIndex(good, "=")+1:],
	} {
		if err := VerifyNMI("secret", header, body); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	if err := VerifyNMI("secret", good, append(body, ' ')); err == nil {
		t.Error("another body verified")
	}
}
