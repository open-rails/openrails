package billing

import (
	"time"

	"github.com/google/uuid"
)

// PSPImportID names one pull of a PSP's subscribers (pim_).
type PSPImportID uuid.UUID

// PSPImportRecordID names one provider object an import staged (pir_).
type PSPImportRecordID uuid.UUID

const (
	PSPImportIDPrefix       = "pim_"
	PSPImportRecordIDPrefix = "pir_"
)

func ParsePSPImportID(s string) (PSPImportID, error) {
	u, err := parsePrefixedID("PSP import", PSPImportIDPrefix, s)
	return PSPImportID(u), err
}

func (id PSPImportID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id PSPImportID) IsZero() bool                 { return uuid.UUID(id) == uuid.Nil }
func (id PSPImportID) String() string               { return formatPrefixedID(PSPImportIDPrefix, uuid.UUID(id)) }
func (id PSPImportID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *PSPImportID) UnmarshalText(text []byte) error {
	parsed, err := ParsePSPImportID(string(text))
	*id = parsed
	return err
}

func ParsePSPImportRecordID(s string) (PSPImportRecordID, error) {
	u, err := parsePrefixedID("PSP import record", PSPImportRecordIDPrefix, s)
	return PSPImportRecordID(u), err
}

func (id PSPImportRecordID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id PSPImportRecordID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id PSPImportRecordID) String() string {
	return formatPrefixedID(PSPImportRecordIDPrefix, uuid.UUID(id))
}
func (id PSPImportRecordID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *PSPImportRecordID) UnmarshalText(text []byte) error {
	parsed, err := ParsePSPImportRecordID(string(text))
	*id = parsed
	return err
}

// PSPImportStatus is where an import is: its pull, then its landing.
type PSPImportStatus string

const (
	PSPImportPulling  PSPImportStatus = "pulling"
	PSPImportPulled   PSPImportStatus = "pulled"
	PSPImportApplying PSPImportStatus = "applying"
	PSPImportApplied  PSPImportStatus = "applied"
	// PSPImportFailed names its error; creating the import again retries.
	PSPImportFailed PSPImportStatus = "failed"
)

// PSPImportRecordKind is what a staged provider object is.
type PSPImportRecordKind string

const (
	// PSPImportCustomer is a provider customer (an NMI vault, a Stripe
	// customer) with its saved cards.
	PSPImportCustomer     PSPImportRecordKind = "customer"
	PSPImportSubscription PSPImportRecordKind = "subscription"
	PSPImportTransaction  PSPImportRecordKind = "transaction"
)

// PSPImportRecordState is how far a staged object got.
type PSPImportRecordState string

const (
	// PSPImportUnmatched: no rule, mapping or link names its customer.
	PSPImportUnmatched PSPImportRecordState = "unmatched"
	// PSPImportConflict: rules name different customers; Reason lists them.
	PSPImportConflict PSPImportRecordState = "conflict"
	PSPImportLinked   PSPImportRecordState = "linked"
	PSPImportImported PSPImportRecordState = "imported"
	// PSPImportBlocked: linked, but it did not land; Reason says why.
	PSPImportBlocked PSPImportRecordState = "blocked"
)

// PSPImportRuleVia is where a rule looks a fact's value up.
type PSPImportRuleVia string

const (
	// PSPImportViaDirectory matches an email or username against the
	// merchant's directory: exactly one customer must hold it.
	PSPImportViaDirectory PSPImportRuleVia = "directory"
	// PSPImportViaMapping reads the merchant's mapping named by Mapping.
	PSPImportViaMapping PSPImportRuleVia = "mapping"
	// PSPImportViaCustomerID takes the value itself as the customer id.
	PSPImportViaCustomerID PSPImportRuleVia = "customer_id"
)

// PSPImportRule links a staged object to a customer by one of its facts.
// Pattern, when set, is the fact's shape with one {key} placeholder (e.g.
// "shipment-{key}"); the captured key is what is looked up. Every rule that
// matches must name the same customer.
type PSPImportRule struct {
	Fact    string           `json:"fact"`
	Pattern string           `json:"pattern,omitempty"`
	Via     PSPImportRuleVia `json:"via"`
	Mapping string           `json:"mapping,omitempty"`
}

// CreatePSPImportParams starts pulling one PSP. HistorySince bounds the
// transaction history read; nil reads from the earliest subscription's start.
// Rules nil links by the directory's email.
type CreatePSPImportParams struct {
	PSP          PSPRef          `json:"psp"`
	HistorySince *time.Time      `json:"history_since,omitempty"`
	Rules        []PSPImportRule `json:"rules,omitempty"`
}

// UpdatePSPImportParams replaces an import's rules; nil keeps them.
type UpdatePSPImportParams struct {
	Rules *[]PSPImportRule `json:"rules,omitempty"`
}

// PSPImportCount is how many staged objects of a kind are in a state.
type PSPImportCount struct {
	Kind    PSPImportRecordKind  `json:"kind"`
	State   PSPImportRecordState `json:"state"`
	Records int                  `json:"records"`
}

// PSPImport is one PSP's pull and landing. HistoryThrough is how far its
// transaction history has been read. Its subscriptions land on the provider
// prices mapping their plans: ListProviderCatalog with Import lists the
// plans to map.
type PSPImport struct {
	ID             PSPImportID      `json:"id"`
	PSP            PSPID            `json:"psp"`
	Rail           string           `json:"rail"`
	Status         PSPImportStatus  `json:"status"`
	HistorySince   *time.Time       `json:"history_since"`
	HistoryThrough *time.Time       `json:"history_through"`
	PulledAt       *time.Time       `json:"pulled_at"`
	AppliedAt      *time.Time       `json:"applied_at"`
	Error          *string          `json:"error"`
	Rules          []PSPImportRule  `json:"rules"`
	Counts         []PSPImportCount `json:"counts"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// PSPImportFact is one identity fact a provider holds for an object.
type PSPImportFact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PSPImportRecord is one staged provider object. Ref is the provider's id;
// CustomerRef and SubscriptionRef name the provider objects it belongs to.
type PSPImportRecord struct {
	ID              PSPImportRecordID    `json:"id"`
	Import          PSPImportID          `json:"import"`
	Kind            PSPImportRecordKind  `json:"kind"`
	Ref             string               `json:"ref"`
	CustomerRef     *string              `json:"customer_ref"`
	SubscriptionRef *string              `json:"subscription_ref"`
	OccurredAt      *time.Time           `json:"occurred_at"`
	Facts           []PSPImportFact      `json:"facts"`
	State           PSPImportRecordState `json:"state"`
	Customer        *CustomerID          `json:"customer"`
	// LinkedVia is how the customer was found: rule:<n>, manual, or the
	// customer or subscription record it belongs to.
	LinkedVia *string `json:"linked_via"`
	// ManualPrice is the provider price picked by hand for a subscription
	// whose provider names no plan (CCBill).
	ManualPrice *PriceID  `json:"manual_price"`
	Reason      *string   `json:"reason"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PSPImportListParams lists the merchant's imports, newest first.
type PSPImportListParams struct {
	PageRequest
	// IDs instead reads 1 to MaxBatchItems named imports in one page.
	IDs []PSPImportID
}

// PSPImportRecordListParams filters an import's records, in id order.
type PSPImportRecordListParams struct {
	PageRequest
	// IDs instead reads 1 to MaxBatchItems named records in one page.
	IDs   []PSPImportRecordID
	State PSPImportRecordState
	Kind  PSPImportRecordKind
}

// PSPImportLink links a record to a customer by hand; a null customer
// removes the link.
type PSPImportLink struct {
	Record   PSPImportRecordID `json:"record"`
	Customer *CustomerID       `json:"customer"`
}

// PSPImportPriceLink picks the provider price a subscription of a CCBill
// import lands on, instead of matching its rebill: a live provider price
// bound to the import's PSP. A null price clears the pick.
type PSPImportPriceLink struct {
	Record PSPImportRecordID `json:"record"`
	Price  *PriceID          `json:"price"`
}

// LinkPSPImportParams applies the import's rules again, with these links and
// price picks. Every record not yet imported is decided again.
type LinkPSPImportParams struct {
	Links  []PSPImportLink      `json:"links,omitempty"`
	Prices []PSPImportPriceLink `json:"prices,omitempty"`
}

// PSPImportMapping maps one of the merchant's own identifiers to a customer;
// a null customer deletes it.
type PSPImportMapping struct {
	Mapping  string      `json:"mapping"`
	Key      string      `json:"key"`
	Customer *CustomerID `json:"customer"`
}

// SetPSPImportMappingsParams writes up to MaxBatchItems mappings.
type SetPSPImportMappingsParams struct {
	Mappings []PSPImportMapping `json:"mappings"`
}

// PSPImportMappingsResult counts the mappings written and deleted.
type PSPImportMappingsResult struct {
	Set     int `json:"set"`
	Deleted int `json:"deleted"`
}
