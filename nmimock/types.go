package nmimock

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Card is what a browser tokenizes and a vault stores.
type Card struct {
	Brand, Last4 string
	// Decline is the issuer's answer: "" approves, an NMI response_code such
	// as "202" declines, and "vault" refuses storing the card.
	Decline string
}

// Vault is a Customer Vault record: its priority-1 billing entry, then Extra.
type Vault struct {
	ID, BillingID string
	Card          Card
	Extra         []Billing
	// NoBrand omits card_type from reads, as some live accounts do.
	NoBrand bool
}

// Billing is a further billing entry (card) in a vault.
type Billing struct {
	ID   string
	Card Card
}

// Plan is a Recurring Plan. Months, when set, is the cadence; otherwise Days
// (30 when zero).
type Plan struct {
	ID, Name, Amount string
	Days, Months     int
}

// Schedule is an NMI recurring subscription. A deleted schedule stays
// readable as a tombstone (delayed_condition=inactive) and leaves lists.
type Schedule struct {
	ID, Vault, Plan, Amount string
	Order                   string
	// Days or Months is the cadence (Days 30 when both are zero).
	Days, Months    int
	NextBilling     time.Time
	Deleted, Paused bool
	// Custom is a custom-amount schedule: its plan shows an empty plan_name.
	Custom bool
}

// Sale is a sale NMI processed, approved or declined.
type Sale struct {
	TransactionID, OrderID, Vault, BillingID string
	// Amount is the wire decimal ("9.99").
	Amount, Currency string
	Card             Card
	// InitiatedBy, Indicator (stored_credential_indicator) and Initial
	// (initial_transaction_id) are the request's credential-on-file fields.
	InitiatedBy, Indicator, Initial string
	ScheduleID                      string
	// Declined is the response_code of a declined sale; "" when approved.
	Declined      string
	RefundedCents int64
	RefundIDs     []string
	// Voided sales never settle: no money moved.
	Voided bool
	At     time.Time
	// Hidden keeps the sale out of the Query API until Reveal.
	Hidden bool
}

// Approved reports whether the sale moved money.
func (s Sale) Approved() bool { return s.Declined == "" }

// Validation is a Direct Post type=validate card verification.
type Validation struct {
	TransactionID, Vault, BillingID string
	Card                            Card
	Approved                        bool
	Form                            url.Values
	At                              time.Time
}

// Call is one journaled mutation: a v5 resource path ("/subscriptions/x")
// or "transact.php" with its form.
type Call struct {
	Method, Path string
	Form         url.Values
	Body         []byte
}

// LedgerEntry is money an approved sale moved and its refunds.
type LedgerEntry struct {
	TransactionID, OrderID, Vault string
	Last4                         string
	Cents, RefundedCents          int64
}

type refund struct {
	ID    string
	Sale  *Sale
	Cents int64
	At    time.Time
}

type recentCharge struct {
	card   Card
	amount string
	at     time.Time
}

func (v *Vault) cardFor(billingID string) (*Card, bool) {
	if billingID == "" || billingID == v.BillingID {
		return &v.Card, true
	}
	for i := range v.Extra {
		if v.Extra[i].ID == billingID {
			return &v.Extra[i].Card, true
		}
	}
	return nil, false
}

// removeBilling promotes the next entry when the primary goes and refuses
// to empty a vault, as NMI does.
func (v *Vault) removeBilling(billingID string) bool {
	if billingID == v.BillingID {
		if len(v.Extra) == 0 {
			return false
		}
		v.BillingID, v.Card, v.Extra = v.Extra[0].ID, v.Extra[0].Card, v.Extra[1:]
		return true
	}
	for i := range v.Extra {
		if v.Extra[i].ID == billingID {
			v.Extra = append(v.Extra[:i], v.Extra[i+1:]...)
			return true
		}
	}
	return false
}

func (v *Vault) clone() Vault {
	c := *v
	c.Extra = append([]Billing(nil), v.Extra...)
	return c
}

// advance is the schedule's next regular billing date after at.
func (s *Schedule) advance(at time.Time) time.Time {
	if s.Months > 0 {
		return at.AddDate(0, s.Months, 0)
	}
	days := s.Days
	if days <= 0 {
		days = 30
	}
	return at.AddDate(0, 0, days)
}

func (s *Sale) clone() Sale {
	c := *s
	c.RefundIDs = append([]string(nil), s.RefundIDs...)
	return c
}

func decimalCents(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// centsOf parses a wire decimal; malformed amounts are a caller bug.
func centsOf(amount string) int64 {
	whole, fraction, _ := strings.Cut(amount, ".")
	if len(fraction) > 2 {
		panic("nmimock: amount has fractional cents: " + amount)
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units < 0 {
		panic("nmimock: invalid amount: " + amount)
	}
	minor, err := strconv.ParseInt(fraction+strings.Repeat("0", 2-len(fraction)), 10, 64)
	if err != nil {
		panic("nmimock: invalid amount: " + amount)
	}
	return units*100 + minor
}

func itoa(n int) string { return strconv.Itoa(n) }
