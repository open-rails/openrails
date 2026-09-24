package sigverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// FC-7/FC-8: HMAC over "t.body", constant-time compare, and a symmetric replay
// window that a non-positive tolerance (queued re-verify) skips.
func TestVerify(t *testing.T) {
	const secret = "whsec"
	body := []byte(`{"id":"evt"}`)
	at := func(d time.Duration) string { return strconv.FormatInt(time.Now().Add(d).Unix(), 10) }
	now, stale, future := at(0), at(-10*time.Minute), at(10*time.Minute)
	stripe := func(ts string, sigs ...string) string {
		h := "t=" + ts
		for _, s := range sigs {
			h += ",v1=" + s
		}
		return h
	}
	nmi := func(ts, sig string) string { return "t=" + ts + ",s=" + sig }

	for _, tc := range []struct {
		name   string
		verify func(string, string, []byte, time.Duration) error
		header string
		body   []byte
		tol    time.Duration
		ok     bool
	}{
		{"stripe fresh", VerifyStripe, stripe(now, sign(secret, now, body)), body, 5 * time.Minute, true},
		{"stripe any of several v1", VerifyStripe, " " + stripe(now, "00", sign(secret, now, body)), body, 5 * time.Minute, true},
		{"stripe tampered body", VerifyStripe, stripe(now, sign(secret, now, body)), []byte(`{"id":"x"}`), 5 * time.Minute, false},
		{"stripe wrong secret", VerifyStripe, stripe(now, sign("other", now, body)), body, 5 * time.Minute, false},
		{"stripe stale", VerifyStripe, stripe(stale, sign(secret, stale, body)), body, 5 * time.Minute, false},
		{"stripe future", VerifyStripe, stripe(future, sign(secret, future, body)), body, 5 * time.Minute, false},
		{"stripe stale, window skipped", VerifyStripe, stripe(stale, sign(secret, stale, body)), body, 0, true},
		{"stripe no v1", VerifyStripe, "t=" + now, body, 0, false},
		{"stripe no t", VerifyStripe, "v1=" + sign(secret, now, body), body, 0, false},
		{"stripe non-numeric t", VerifyStripe, stripe("nonce", sign(secret, "nonce", body)), body, 0, false},
		{"nmi fresh", VerifyNMI, nmi(now, sign(secret, now, body)), body, 5 * time.Minute, true},
		{"nmi quoted upper-case hex", VerifyNMI, `s="` + strings.ToUpper(sign(secret, now, body)) + `", t='` + now + `'`, body, 5 * time.Minute, true},
		{"nmi tampered body", VerifyNMI, nmi(now, sign(secret, now, body)), []byte("x"), 5 * time.Minute, false},
		{"nmi stale", VerifyNMI, nmi(stale, sign(secret, stale, body)), body, 5 * time.Minute, false},
		{"nmi future", VerifyNMI, nmi(future, sign(secret, future, body)), body, 5 * time.Minute, false},
		{"nmi stale, window skipped", VerifyNMI, nmi(stale, sign(secret, stale, body)), body, -1, true},
		{"nmi non-hex signature", VerifyNMI, nmi(now, "zz"), body, 0, false},
		{"nmi truncated signature", VerifyNMI, nmi(now, sign(secret, now, body)[:62]), body, 0, false},
		{"nmi garbage", VerifyNMI, "garbage", body, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.verify(secret, tc.header, tc.body, tc.tol)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
