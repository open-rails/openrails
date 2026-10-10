package paymentmethods

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/mandates"
)

// Method statuses. A method is chargeable only while active; the others are
// final.
const (
	StatusActive   = "active"
	StatusClosed   = "closed"
	StatusReplaced = "replaced"
	StatusRemoved  = "removed"
)

// CardSource is who reported a change to a stored card (#1168).
type CardSource string

const (
	SourceCustomerSave       CardSource = "customer_save"
	SourceCustomerEdit       CardSource = "customer_edit"
	SourceStripeUpdater      CardSource = "stripe_updater"
	SourceNMIACU             CardSource = "nmi_acu"
	SourceBasisTheoryUpdater CardSource = "basis_theory_updater"
	SourceHyperswitchUpdater CardSource = "hyperswitch_updater"
	SourceNetworkToken       CardSource = "network_token"
	// SourceProviderRead: OpenRails read the holder and found the card changed.
	SourceProviderRead CardSource = "provider_read"
)

// CardChange is what a card version records.
type CardChange string

const (
	CardSaved CardChange = "saved"
	// CardUpdated: a new number, expiry, holder handle or network token under
	// the same brand. Billing continues under the same agreements.
	CardUpdated CardChange = "updated"
	// CardBrandChanged: reissued under another brand. No network lineage
	// survives, so the card's mandates wait for the customer's consent.
	CardBrandChanged CardChange = "brand_changed"
	// CardClosed: the bank closed the account. Final; its mandates end.
	CardClosed CardChange = "closed"
	// CardContactCardholder: the issuer asks for the cardholder. The card
	// keeps billing; the customer is prompted.
	CardContactCardholder CardChange = "contact_cardholder"
)

// NeedsCustomer reports a change only the customer can resolve.
func (c CardChange) NeedsCustomer() bool {
	return c == CardBrandChanged || c == CardClosed || c == CardContactCardholder
}

// CardAdvice is an issuer's report about a card that carries no new card.
type CardAdvice string

const (
	AdviceClosed CardAdvice = "closed"
	// AdviceContactCardholder is a processor's contact-cardholder answer.
	// Mastercard's updater means a closed account by it (ABU CONTACT), so it
	// is normalized to closed on a Mastercard card.
	AdviceContactCardholder CardAdvice = "contact_cardholder"
)

// Card is a stored card as its holder reports it. An empty field was not
// reported and keeps what the method holds.
type Card struct {
	models.Card
	Fingerprint                                         string
	RailCustomerRef, RailMethodRef                      string
	NetworkTokenID, NetworkTokenStatus, NetworkTokenPAR string
}

// CardEvent is one report about a stored card: what its holder now holds, or
// an advice.
type CardEvent struct {
	MerchantID, PaymentMethodID uuid.UUID
	Source                      CardSource
	// EventRef names the notice, event or operation; a replay changes nothing.
	EventRef string
	Card     Card
	Advice   CardAdvice
	At       time.Time
}

// CardLifecycle is what ApplyCardLifecycle did.
type CardLifecycle struct {
	// Method is the method as it stands after the event.
	Method gen.BillingPaymentMethod
	// Change is the version written; empty when nothing changed, the event
	// was a replay, or the method's status is final.
	Change CardChange
	// Reissued: the card's number, expiry or holder handle changed, so
	// billing waiting on the old card can resume.
	Reissued bool
	// Replayed: the source's event was already applied.
	Replayed bool
}

// HeldCard is a holder's answer about one stored card: the card it holds and
// who to credit with any change, or Gone when it no longer holds it.
type HeldCard struct {
	Card   Card
	Source CardSource
	Gone   bool
}

// CardRefreshArgs is the River job that reads one declined card from its
// holder before its members are asked for another.
type CardRefreshArgs struct {
	MerchantID      uuid.UUID `json:"merchant_id"`
	PaymentMethodID uuid.UUID `json:"payment_method_id"`
}

// KindCardRefresh is CardRefreshArgs' River kind.
const KindCardRefresh = "openrails.card_refresh"

// CardRefreshAttempts bounds the holder reads before the members are asked
// anyway: an unreadable holder never keeps a customer from hearing.
const CardRefreshAttempts = 4

func (CardRefreshArgs) Kind() string { return KindCardRefresh }

// InsertOpts collapses the reads one card's declines queue within an hour.
func (CardRefreshArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: CardRefreshAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Hour}}
}

// CardHolders reads stored cards from the PSP or custodian holding them.
type CardHolders interface {
	ReadHeldCard(ctx context.Context, method gen.BillingPaymentMethod) (HeldCard, error)
}

// ApplyCardLifecycle is the one writer of a stored card's facts and standing
// for every source (#1168). Inside the caller's transaction it locks the
// method, writes a card version and applies the event:
//   - a same-brand reissue updates the card and keeps its mandates;
//   - another brand updates the card and holds its mandates for reconsent;
//   - a closed account closes the method and ends its mandates; a closed card
//     parks the subscriptions it pays and never cancels them;
//   - a contact-cardholder advice flags the method and keeps it billing.
//
// A method whose status is final takes no further versions.
func ApplyCardLifecycle(ctx context.Context, q *gen.Queries, ev CardEvent) (CardLifecycle, error) {
	ev.EventRef = strings.TrimSpace(ev.EventRef)
	if ev.MerchantID == uuid.Nil || ev.PaymentMethodID == uuid.Nil || ev.Source == "" || ev.EventRef == "" || ev.At.IsZero() {
		return CardLifecycle{}, errors.New("card lifecycle: merchant, method, source, event and time are required")
	}
	at := ev.At.UTC()
	m, err := q.GetPaymentMethodForUpdate(ctx, gen.GetPaymentMethodForUpdateParams{MerchantID: ev.MerchantID, ID: ev.PaymentMethodID})
	if err != nil {
		return CardLifecycle{}, err
	}
	out := CardLifecycle{Method: m}
	if out.Replayed, err = q.PaymentMethodVersionRecorded(ctx, gen.PaymentMethodVersionRecordedParams{MerchantID: ev.MerchantID, PaymentMethodID: m.ID, Source: string(ev.Source), EventRef: ev.EventRef}); err != nil || out.Replayed || m.Status != StatusActive {
		return out, err
	}
	next := m
	switch ev.Advice {
	case AdviceClosed:
		out.Change = CardClosed
	case AdviceContactCardholder:
		out.Change = CardContactCardholder
		if cardNetwork(models.DerefStr(m.CardBrand)) == "mastercard" {
			out.Change = CardClosed
		}
	case "":
		next, out.Change, out.Reissued = observe(m, ev.Card)
		if out.Change == "" {
			return out, nil
		}
	default:
		return out, fmt.Errorf("card lifecycle: unknown advice %q", ev.Advice)
	}
	switch out.Change {
	case CardClosed:
		next.Status = StatusClosed
		if _, err := mandates.EndForPaymentMethod(ctx, q, ev.MerchantID, m.ID, mandates.EndClosed, at); err != nil {
			return out, err
		}
	case CardContactCardholder:
		next.ContactCardholderAt = &at
	case CardBrandChanged:
		if err := mandates.RequireReconsent(ctx, q, ev.MerchantID, m.ID, at); err != nil {
			return out, err
		}
		fallthrough
	case CardUpdated:
		if out.Reissued || out.Change == CardBrandChanged {
			next.ContactCardholderAt = nil
			// or#872: the issuer's newest word about the card lifts a hold an
			// older one placed; a deletion in flight stays fenced.
			if !strings.HasPrefix(models.DerefStr(m.ParkReason), "delete:") {
				next.ParkReason, next.ParkedAt = nil, nil
			}
		}
	}
	n, err := q.SetPaymentMethodCard(ctx, gen.SetPaymentMethodCardParams{
		MerchantID: ev.MerchantID, ID: m.ID,
		RailCustomerRef: next.RailCustomerRef, RailMethodRef: next.RailMethodRef,
		CardBrand: next.CardBrand, CardLast4: next.CardLast4, CardExpMonth: next.CardExpMonth, CardExpYear: next.CardExpYear,
		Fingerprint: next.Fingerprint, NetworkTokenID: next.NetworkTokenID, NetworkTokenStatus: next.NetworkTokenStatus, NetworkTokenPar: next.NetworkTokenPar,
		Status: next.Status, ContactCardholderAt: next.ContactCardholderAt, ParkReason: next.ParkReason, ParkedAt: next.ParkedAt, UpdatedAt: at,
	})
	if err != nil {
		return out, err
	}
	if n != 1 {
		return out, ErrPaymentMethodNotFound
	}
	next.UpdatedAt = at
	out.Method = next
	return out, RecordVersion(ctx, q, next, ev.Source, out.Change, ev.EventRef, at)
}

// RecordVersion writes m's card as a version; a replay of source's event
// records nothing.
func RecordVersion(ctx context.Context, q *gen.Queries, m gen.BillingPaymentMethod, source CardSource, change CardChange, eventRef string, at time.Time) error {
	brand := m.CardBrand
	if models.DerefStr(brand) == "" {
		brand = nil
	}
	_, err := q.InsertPaymentMethodVersion(ctx, gen.InsertPaymentMethodVersionParams{
		MerchantID: m.MerchantID, CustomerID: m.CustomerID, PaymentMethodID: m.ID,
		Source: string(source), Kind: string(change), EventRef: strings.TrimSpace(eventRef),
		PspID: m.PspID, CustodianID: m.CustodianID, RailCustomerRef: m.RailCustomerRef, RailMethodRef: m.RailMethodRef,
		CardBrand: brand, CardLast4: m.CardLast4, CardExpMonth: m.CardExpMonth, CardExpYear: m.CardExpYear, Fingerprint: m.Fingerprint,
		NetworkTokenID: m.NetworkTokenID, NetworkTokenStatus: m.NetworkTokenStatus, NetworkTokenPar: m.NetworkTokenPar,
		EffectiveAt: at.UTC(),
	})
	return err
}

// observe merges what a holder reports into the method and names the change.
func observe(m gen.BillingPaymentMethod, c Card) (gen.BillingPaymentMethod, CardChange, bool) {
	next := m
	set := func(dst **string, v string) bool {
		v = strings.TrimSpace(v)
		if v == "" || models.DerefStr(*dst) == v {
			return false
		}
		*dst = &v
		return true
	}
	brandChanged := false
	if brand := strings.TrimSpace(c.Brand); brand != "" {
		was := models.DerefStr(m.CardBrand)
		brandChanged = BrandChanged(was, brand)
		// The same network spelled another way keeps the stored spelling.
		if brandChanged || cardNetwork(was) == "" || cardNetwork(was) != cardNetwork(brand) {
			set(&next.CardBrand, brand)
		}
	}
	reissued := false
	if c.Last4 != "" && set(&next.CardLast4, c.Last4) {
		reissued = true
	}
	if set(&next.Fingerprint, c.Fingerprint) {
		reissued = true
	}
	if c.ExpMonth >= 1 && c.ExpMonth <= 12 && c.ExpYear >= 2000 && c.ExpYear <= 2199 {
		month, year := int16(c.ExpMonth), int16(c.ExpYear)
		if m.CardExpMonth == nil || m.CardExpYear == nil || *m.CardExpMonth != month || *m.CardExpYear != year {
			next.CardExpMonth, next.CardExpYear = &month, &year
			reissued = true
		}
	}
	if set(&next.RailCustomerRef, c.RailCustomerRef) {
		reissued = true
	}
	if set(&next.RailMethodRef, c.RailMethodRef) {
		reissued = true
	}
	token := set(&next.NetworkTokenID, c.NetworkTokenID)
	token = set(&next.NetworkTokenStatus, c.NetworkTokenStatus) || token
	token = set(&next.NetworkTokenPar, c.NetworkTokenPAR) || token
	switch {
	case brandChanged:
		return next, CardBrandChanged, true
	case reissued || token || models.DerefStr(next.CardBrand) != models.DerefStr(m.CardBrand):
		return next, CardUpdated, reissued
	}
	return m, "", false
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

// Chargeable reports whether a method can be charged now: active, with no
// holder-side hold.
func Chargeable(m gen.BillingPaymentMethod) bool {
	return m.Status == StatusActive && m.ParkReason == nil
}

// MergeBillingDetails applies a customer's billing details to a card's stored
// ones: a field given replaces, an empty one clears, an absent one stays.
func MergeBillingDetails(metadata map[string]any, d *billing.BillingDetails) map[string]any {
	out := map[string]any{}
	for k, v := range metadata {
		out[k] = v
	}
	if d == nil {
		return out
	}
	set := func(key string, value *string) {
		if value == nil {
			return
		}
		if v := strings.TrimSpace(*value); v != "" {
			out[key] = v
		} else {
			delete(out, key)
		}
	}
	set("name_on_card", d.Name)
	set("billing_email", d.Email)
	set("billing_phone", d.Phone)
	if a := d.Address; a != nil {
		set("billing_address1", a.Line1)
		set("billing_address2", a.Line2)
		set("billing_city", a.City)
		set("billing_state", a.State)
		set("postal_code", a.PostalCode)
		set("billing_country", a.Country)
	}
	return out
}
