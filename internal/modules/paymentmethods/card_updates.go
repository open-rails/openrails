package paymentmethods

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// CardUpdateSource is who changed a stored card's standing (#1115).
type CardUpdateSource string

const (
	UpdateNMIACU           CardUpdateSource = "nmi_acu"
	UpdateBTAccountUpdater CardUpdateSource = "bt_account_updater"
	UpdateStripe           CardUpdateSource = "stripe_card_updater"
	UpdateByCustomer       CardUpdateSource = "customer"
)

// CardUpdateKind is what changed.
type CardUpdateKind string

const (
	CardUpdated CardUpdateKind = "updated"
	// CardBrandChanged: reissued under another brand, which voids the card's
	// stored-credential agreements (#1166).
	CardBrandChanged CardUpdateKind = "brand_changed"
	CardClosed       CardUpdateKind = "closed_account"
	ContactCustomer  CardUpdateKind = "contact_customer"
)

// CardUpdate is one change to a stored card's standing. PSPID is the PSP
// holding a PSP-held card, nil for a custodian-held one.
type CardUpdate struct {
	MerchantID, PaymentMethodID, CustomerID uuid.UUID
	PSPID                                   *uuid.UUID
	Source                                  CardUpdateSource
	Kind                                    CardUpdateKind
	// EventRef names the notice or operation; a replay records nothing.
	EventRef string
	// OccurredAt is when the change was learned; zero is the database's now.
	OccurredAt time.Time
}

// RecordCardUpdate records u once.
func RecordCardUpdate(ctx context.Context, q *gen.Queries, u CardUpdate) error {
	p := gen.InsertPaymentMethodUpdateParams{
		MerchantID: u.MerchantID, PaymentMethodID: u.PaymentMethodID, CustomerID: u.CustomerID, PspID: u.PSPID,
		Source: string(u.Source), Kind: string(u.Kind), EventRef: u.EventRef,
	}
	if !u.OccurredAt.IsZero() {
		at := u.OccurredAt.UTC()
		p.OccurredAt = &at
	}
	return q.InsertPaymentMethodUpdate(ctx, p)
}

// BrandChanged reports whether a reissue moved a card to another brand. A
// brand either side does not name, or does not recognize, never counts:
// voiding a card's agreements on a misread spelling would stop its billing.
func BrandChanged(from, to string) bool {
	a, b := cardNetwork(from), cardNetwork(to)
	return a != "" && b != "" && a != b
}

func cardNetwork(brand string) string {
	var key strings.Builder
	for _, r := range strings.ToLower(brand) {
		if r >= 'a' && r <= 'z' {
			key.WriteRune(r)
		}
	}
	switch key.String() {
	case "visa":
		return "visa"
	case "mastercard", "mc":
		return "mastercard"
	case "amex", "americanexpress":
		return "amex"
	case "discover":
		return "discover"
	case "diners", "dinersclub":
		return "diners"
	case "jcb":
		return "jcb"
	case "unionpay", "cup", "chinaunionpay":
		return "unionpay"
	}
	return ""
}
