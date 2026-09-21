package charge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// CustomerPaymentKey scopes a caller's opaque key to the verified payer and
// command kind. Resource/body binding is checked against the accepted payload.
func CustomerPaymentKey(kind string, payer uuid.UUID, key string) string {
	digest := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s:customer:%s:%x", kind, payer, digest)
}
func CustomerPaymentKeyValid(kind string, payer uuid.UUID, key string) bool {
	prefix := fmt.Sprintf("%s:customer:%s:", kind, payer)
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(key, prefix))
	return err == nil && len(digest) == sha256.Size
}
