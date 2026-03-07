package webhooksig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVerifyStripeSignature(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"evt_123","type":"checkout.session.completed"}`)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	signature := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%s,v1=%s", timestamp, signature)

	require.NoError(t, VerifyStripeSignature(secret, header, body, time.Minute))

	err := VerifyStripeSignature(secret, fmt.Sprintf("t=%s,v1=bad", timestamp), body, time.Minute)
	require.Error(t, err)
	require.ErrorContains(t, err, "stripe signature mismatch")
}
