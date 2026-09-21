package grants

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
)

// PurchaseWindow is an accepted access interval, separate from recording time.
type PurchaseWindow struct {
	Start time.Time
	End   *time.Time
}

func samePurchaseEnd(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

func PurchaseWindows(spec map[string]*int, duration *int, accepted, entitlementStart time.Time) (map[string]PurchaseWindow, PurchaseWindow) {
	wanted := make(map[string]PurchaseWindow, len(spec))
	for name, hours := range spec {
		var end *time.Time
		if duration != nil && *duration > 0 {
			v := entitlementStart.Add(time.Duration(*duration) * time.Hour)
			end = &v
		} else if duration == nil && hours != nil && *hours > 0 {
			v := entitlementStart.Add(time.Duration(*hours) * time.Hour)
			end = &v
		}
		wanted[name] = PurchaseWindow{Start: entitlementStart, End: end}
	}
	ownership := PurchaseWindow{Start: accepted}
	if duration != nil && *duration > 0 {
		v := accepted.Add(time.Duration(*duration) * time.Hour)
		ownership.End = &v
	}
	return wanted, ownership
}

type PurchaseHistory struct {
	Entitlements map[string]gen.OpenrailsGrant
	Ownership    *gen.OpenrailsGrant
}

// ValidatePurchaseHistory compares original immutable grant events. A later
// revoke never changes those facts and must not be mistaken for missing access.
// Missing effects are returned to the caller; completion can repair them, while
// an archive requiring a complete terminal purchase refuses them.
func ValidatePurchaseHistory(merchant, customer, product, payment uuid.UUID, wanted map[string]PurchaseWindow, ownership PurchaseWindow, original []gen.OpenrailsGrant) (PurchaseHistory, error) {
	out := PurchaseHistory{Entitlements: map[string]gen.OpenrailsGrant{}}
	for _, g := range original {
		if g.MerchantID != merchant || g.CustomerID != customer || g.Event != "grant" || g.SourceType != string(Purchase) || g.SourceID != payment.String() || g.PaymentID != nil && *g.PaymentID != payment || g.ProductID != nil && *g.ProductID != product {
			return out, errors.New("original grant belongs to another accepted purchase")
		}
		switch g.Kind {
		case string(Ownership):
			if out.Ownership != nil || g.ProductID == nil || !g.StartsAt.Equal(ownership.Start) || !samePurchaseEnd(g.EndsAt, ownership.End) {
				return out, errors.New("original ownership window contradicts accepted purchase")
			}
			copy := g
			out.Ownership = &copy
		case string(Entitlement):
			var spec Spec
			if err := json.Unmarshal(g.SpecSnapshot, &spec); err != nil || len(spec.Entitlements) == 0 || spec.Deposit != nil {
				return out, errors.New("original entitlement spec contradicts accepted purchase")
			}
			for _, name := range spec.Entitlements {
				window, ok := wanted[name]
				if _, duplicate := out.Entitlements[name]; !ok || duplicate || !g.StartsAt.Equal(window.Start) || !samePurchaseEnd(g.EndsAt, window.End) {
					return out, errors.New("original entitlement window contradicts accepted purchase")
				}
				out.Entitlements[name] = g
			}
		default:
			return out, errors.New("purchase has an unsupported original grant")
		}
	}
	return out, nil
}
