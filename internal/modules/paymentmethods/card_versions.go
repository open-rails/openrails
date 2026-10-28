package paymentmethods

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/history"
	"github.com/open-rails/openrails/internal/modules/mandates"
)

// Method statuses. A method is chargeable only while active; the others are
// final.
const (
	// StatusRequiresAction is a card being saved: the customer completes the
	// bank's authentication first. It is never charged.
	StatusRequiresAction = "requires_action"
	StatusActive         = "active"
	StatusClosed         = "closed"
	StatusReplaced       = "replaced"
	StatusRemoved        = "removed"
)

// CardSource is who reported a change to a stored card.
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

// StoredCardRefreshArgs is the River job that asks a stored card's holder for
// its current card. Only holders that must be asked (a Basis Theory vault)
// have one; a holder that updates its cards in place never does.
type StoredCardRefreshArgs struct {
	MerchantID      uuid.UUID `json:"merchant_id" river:"unique"`
	PaymentMethodID uuid.UUID `json:"payment_method_id" river:"unique"`
	// Declined: a charge declined as an expired or reissued card. A refresh
	// that finds no new card then asks the customer for one.
	Declined bool `json:"declined" river:"unique"`
	// MethodRef and CheckedAt are the card as queued: the handle charged (or
	// found due) and its last check. A card reissued or checked since needs no
	// second request.
	MethodRef string     `json:"method_ref,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// StoredCardRefreshFor queues a refresh of method as it stands, or of the
// handle a declined charge used.
func StoredCardRefreshFor(method gen.BillingPaymentMethod, declined bool, chargedRef string) StoredCardRefreshArgs {
	ref := models.DerefStr(method.RailMethodRef)
	if chargedRef != "" {
		ref = chargedRef
	}
	return StoredCardRefreshArgs{MerchantID: method.MerchantID, PaymentMethodID: method.ID, Declined: declined, MethodRef: ref, CheckedAt: method.AccountUpdaterCheckedAt}
}

// Moot reports a refresh overtaken since it was queued: the card was
// reissued (another handle) or checked again.
func (a StoredCardRefreshArgs) Moot(method gen.BillingPaymentMethod) (reissued, checked bool) {
	if a.MethodRef != "" && a.MethodRef != models.DerefStr(method.RailMethodRef) {
		return true, false
	}
	now := method.AccountUpdaterCheckedAt
	return false, now != nil && (a.CheckedAt == nil || now.UnixMicro() != a.CheckedAt.UnixMicro())
}

// KindStoredCardRefresh is StoredCardRefreshArgs' River kind.
const KindStoredCardRefresh = "openrails.stored_card_refresh"

// StoredCardRefreshAttempts bounds the tries before a declined card's customer
// is asked anyway: an unreachable holder never keeps a customer from hearing.
const StoredCardRefreshAttempts = 4

func (StoredCardRefreshArgs) Kind() string { return KindStoredCardRefresh }

// InsertOpts collapses one card's queued refreshes; a finished one does not
// block the next.
func (StoredCardRefreshArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: StoredCardRefreshAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
	}}}
}

// ErrAccountUpdateRefused is a holder's definitive refusal of an update
// request; repeating it changes nothing.
var ErrAccountUpdateRefused = errors.New("account update refused")

// AccountUpdaters arms the account updater of a stored card's holder. ok is
// false for a holder that updates its cards in place, an unarmed vault or a
// card network the updater does not cover.
type AccountUpdaters interface {
	AccountUpdater(ctx context.Context, method gen.BillingPaymentMethod) (AccountUpdater, bool, error)
}

// AccountUpdater asks a holder for one stored card's current card. A repeated
// key returns the first answer.
type AccountUpdater interface {
	Update(ctx context.Context, key string) (AccountUpdate, error)
}

// AccountUpdate is a holder's answer: its verbatim result code and either the
// reissued card (RailMethodRef is the holder's new handle) or an advice.
// Neither means no change.
type AccountUpdate struct {
	ResultCode string
	Card       Card
	Advice     CardAdvice
}

// Empty reports an answer that changes nothing.
func (u AccountUpdate) Empty() bool { return u.Advice == "" && u.Card == (Card{}) }

// ApplyCardLifecycle is the one writer of a stored card's facts and standing
// for every source. Inside the caller's transaction it locks the method,
// writes a card version and applies the event:
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
		if CardNetwork(models.DerefStr(m.CardBrand)) == "mastercard" {
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
			// The issuer's newest word about the card lifts a hold an older
			// one placed; a deletion in flight stays fenced.
			if !strings.HasPrefix(models.DerefStr(m.ParkReason), "delete:") {
				next.ParkReason, next.ParkedAt = nil, nil
			}
		}
	}
	// Snapshot affected subscriptions before closing the card releases its default.
	// Both the version and the actual card write remain in the caller's transaction.
	if err := RecordVersion(ctx, q, next, ev.Source, out.Change, ev.EventRef, at); err != nil {
		return out, err
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
	return out, nil
}

// RecordVersion writes m's card as a version; a replay of source's event
// records nothing.
func RecordVersion(ctx context.Context, q *gen.Queries, m gen.BillingPaymentMethod, source CardSource, change CardChange, eventRef string, at time.Time) error {
	brand := m.CardBrand
	if models.DerefStr(brand) == "" {
		brand = nil
	}
	_, err := q.InsertPaymentMethodVersion(ctx, gen.InsertPaymentMethodVersionParams{
		HistoryAttribution: history.Bind(ctx, m.MerchantID, uuid.Nil, "card_version", 0, string(change)),
		MerchantID:         m.MerchantID, CustomerID: m.CustomerID, PaymentMethodID: m.ID,
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
		if brandChanged || CardNetwork(was) == "" || CardNetwork(was) != CardNetwork(brand) {
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
	a, b := CardNetwork(from), CardNetwork(to)
	return a != "" && b != "" && a != b
}

// CardNetwork names a brand's card network; "" when unrecognized.
func CardNetwork(brand string) string {
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
