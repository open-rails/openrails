package grants

import (
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

// AccessWindow is a purchase's access: from start for duration hours, or
// indefinitely when the price has no access duration.
func AccessWindow(duration *int, start time.Time) PurchaseWindow {
	window := PurchaseWindow{Start: start}
	if duration != nil && *duration > 0 {
		end := start.Add(time.Duration(*duration) * time.Hour)
		window.End = &end
	}
	return window
}

// SameWindow reports whether a grant records exactly the window.
func SameWindow(g gen.BillingGrant, w PurchaseWindow) bool {
	return g.StartsAt.Equal(w.Start) && (g.EndsAt == nil && w.End == nil || g.EndsAt != nil && w.End != nil && g.EndsAt.Equal(*w.End))
}

// RecordedPurchaseAccess finds the purchase's access grant among its original
// immutable events. A later revoke never changes that fact and is not missing
// access. Entitlement and ownership events from before product access are
// superseded history. nil: the purchase has not been granted yet.
func RecordedPurchaseAccess(merchant, customer, product, payment uuid.UUID, original []gen.BillingGrant) (*gen.BillingGrant, error) {
	var found *gen.BillingGrant
	for _, g := range original {
		if g.MerchantID != merchant || g.CustomerID != customer || g.Event != "grant" || g.SourceType != string(Purchase) || sourceIDOf(g) != payment.String() || g.PaymentID != nil && *g.PaymentID != payment || g.ProductID != nil && *g.ProductID != product {
			return nil, errors.New("original grant belongs to another accepted purchase")
		}
		switch g.Kind {
		case string(Access):
			if found != nil {
				return nil, errors.New("purchase has two access grants")
			}
			copy := g
			found = &copy
		case string(Entitlement), string(Ownership):
		default:
			return nil, errors.New("purchase has an unsupported original grant")
		}
	}
	return found, nil
}

// MigrationReason is the note on grants the product access cutover converted.
const MigrationReason = "product access migration"

// Migrated reports whether the product access cutover converted the grant
// from per-key windows or a product ownership grant.
func Migrated(g gen.BillingGrant) bool {
	return g.Reason != nil && *g.Reason == MigrationReason
}
