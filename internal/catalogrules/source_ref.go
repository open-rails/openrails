package catalogrules

import (
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// SourceRef spells a polymorphic entitlement or grant source in its kind's
// wire form, so source_id beside source_type is the id the source resource
// itself carries: subscription and grace sources are SubscriptionIDs, purchase
// sources are PaymentIDs. Any other source (admin) is the host's
// own declared id, verbatim.
func SourceRef(sourceType, id string) string {
	u, err := uuid.Parse(id)
	if err != nil {
		return id
	}
	switch sourceType {
	case "subscription", "grace":
		return billing.SubscriptionID(u).String()
	case "purchase":
		return billing.PaymentID(u).String()
	}
	return id
}
