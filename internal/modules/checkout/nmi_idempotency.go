package checkout

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// nmiOrderIDSuffix derives a short deterministic suffix from a value, so an NMI
// order id stays stable across retries of the same logical charge.
func nmiOrderIDSuffix(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:4])
}
