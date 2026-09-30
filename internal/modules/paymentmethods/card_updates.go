package paymentmethods

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// CardUpdateSource is who changed a stored card's standing (#1115).
type CardUpdateSource string

const (
	UpdateNMIACU           CardUpdateSource = "nmi_acu"
	UpdateBTAccountUpdater CardUpdateSource = "bt_account_updater"
	UpdateByCustomer       CardUpdateSource = "customer"
)

// CardUpdateKind is what changed.
type CardUpdateKind string

const (
	CardUpdated     CardUpdateKind = "updated"
	CardClosed      CardUpdateKind = "closed_account"
	ContactCustomer CardUpdateKind = "contact_customer"
)

// CardUpdate is one change to a stored card's standing.
type CardUpdate struct {
	MerchantID, PaymentMethodID, CustomerID, PSPID uuid.UUID
	Source                                         CardUpdateSource
	Kind                                           CardUpdateKind
	// EventRef names the notice or operation; a replay records nothing.
	EventRef string
	// At is when the change was learned; zero is the database's now.
	At time.Time
}

// RecordCardUpdate records u once.
func RecordCardUpdate(ctx context.Context, q *gen.Queries, u CardUpdate) error {
	p := gen.InsertPaymentMethodUpdateParams{
		MerchantID: u.MerchantID, PaymentMethodID: u.PaymentMethodID, CustomerID: u.CustomerID, PspID: u.PSPID,
		Source: string(u.Source), Kind: string(u.Kind), EventRef: u.EventRef,
	}
	if !u.At.IsZero() {
		at := u.At.UTC()
		p.At = &at
	}
	return q.InsertPaymentMethodUpdate(ctx, p)
}
