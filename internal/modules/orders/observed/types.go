// Package observed records verified provider bills and money locally. It
// never executes collection or creates subscription entitlements.
package observed

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

const (
	OwnerEngine      = "engine"
	OwnerNMISchedule = "nmi_schedule"
	OwnerProvider    = "provider"
)

// ErrConflict means an already accepted identity was presented with different
// immutable terms. The caller retains the observation for reconciliation.
var ErrConflict = errors.New("provider bill identity conflicts with accepted terms")

// Bill is a verified, finalized obligation. SourceKind/SourceKey identify the
// bill within PSPID; a transaction or webhook ID alone does not identify it.
// Record requires a transaction-bound DB. Repeated calls may append distinct
// actual payments to the same bill, without repeating its completion event.
type Bill struct {
	PSPID, CustomerID             uuid.UUID
	SubscriptionID                *uuid.UUID
	ExistingOrderID               *uuid.UUID
	Owner, Kind                   string
	SourceKind, SourceKey         string
	Currency                      string
	Total                         int64
	PeriodStart, PeriodEnd, DueAt time.Time
	Lines                         []Line
	ProviderReference             string
	// Payment is one actual movement; Record supplies its order reference.
	// Nil records the bill without inventing gateway money.
	Payment *models.Payment
	// Settled is verified provider evidence that the obligation is paid.
	// SettlementSource distinguishes cash, credit, out_of_band, and mixed.
	Settled          bool
	SettlementSource string
	// NonCashAmount is the verified amount settled without gateway money.
	NonCashAmount int64
}

// Line retains the accepted terms. ProviderReference is the provider line
// identity. A source line records an explicit adjustment without pretending
// it was a catalog price. Amount is authoritative, including seat rounding.
type Line struct {
	ProviderAccessDefault                     bool `json:"ProviderAccessDefault,omitempty"`
	PriceID, ProductID                        *uuid.UUID
	Description                               string
	Quantity                                  *int
	UnitAmount, Amount                        int64
	Source                                    string
	BillingIntervalHours, AccessDurationHours *int
	Ownership, ClaimKey                       string
	ProviderReference                         string
}

// Result distinguishes a replay from new local facts. Completed is true only
// for the first completion transition; recording another real capture never
// grants another period or emits another completion.
type Result struct {
	Order                              gen.BillingOrder
	Payment                            *models.Payment
	Created, PaymentCreated, Completed bool
}
