package checkout

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func nmiIdempotentOrderID(prefix, key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func nmiOrderIDSuffix(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:4])
}
