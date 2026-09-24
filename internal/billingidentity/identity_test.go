package billingidentity

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// There is no synthesized stand-in: anything but a UUID is the zero id.
func TestCustomerIDFromStringIsUUIDOnly(t *testing.T) {
	u := uuid.New()
	for in, want := range map[string]uuid.UUID{
		u.String():                        u,
		" " + strings.ToUpper(u.String()): u,
		"":                                uuid.Nil,
		"   ":                             uuid.Nil,
		"not-a-uuid":                      uuid.Nil,
	} {
		if got := CustomerIDFromString(in).UUID(); got != want {
			t.Errorf("CustomerIDFromString(%q) = %s, want %s", in, got, want)
		}
	}
}

// Unknown invoker types fail closed into the stricter delegated cutoffs.
func TestInvokerTypeFailsClosed(t *testing.T) {
	for in, payer := range map[string]bool{"payer": true, " payer ": true, "PAYER": false, "": false, "delegated": false, "admin": false} {
		if IsDirectPayerInvoker(in) != payer {
			t.Errorf("IsDirectPayerInvoker(%q) = %v", in, !payer)
		}
	}
}
