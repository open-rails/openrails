package charge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// CustomerPaymentKey scopes a caller's opaque key to the verified customer and
// command kind. Resource/body binding is checked against the accepted payload.
func CustomerPaymentKey(kind string, customer uuid.UUID, key string) string {
	digest := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s:customer:%s:%x", kind, customer, digest)
}
func CustomerPaymentKeyValid(kind string, customer uuid.UUID, key string) bool {
	prefix := fmt.Sprintf("%s:customer:%s:", kind, customer)
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	encoded := strings.TrimPrefix(key, prefix)
	if encoded != strings.ToLower(encoded) {
		return false
	}
	digest, err := hex.DecodeString(encoded)
	return err == nil && len(digest) == sha256.Size
}
