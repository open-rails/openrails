package intents

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// CustomerPaymentKey scopes a caller's opaque key to the verified payer and
// command kind. Resource/body binding is checked against the accepted payload.
func CustomerPaymentKey(kind string, payer uuid.UUID, key string) string {
	return fmt.Sprintf("%s:customer:%s:%s", kind, payer, uuid.NewSHA1(payer, []byte(key)))
}
func customerPaymentKeyValid(kind string, payer uuid.UUID, key string) bool {
	prefix := fmt.Sprintf("%s:customer:%s:", kind, payer)
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	id, err := uuid.Parse(strings.TrimPrefix(key, prefix))
	return err == nil && id != uuid.Nil
}
