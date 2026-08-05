package rails

import (
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// CancelMode classifies how a subscriber-facing cancellation behaves on a rail.
type CancelMode string

const (
	// CancelModeReversible: cancellation can be undone in-place before the paid
	// period ends (Stripe's cancel_at_period_end; NMI while a deferred delete is
	// still pending — issue 216).
	CancelModeReversible CancelMode = "reversible"

	// CancelModeDestructive: cancellation tears down the rail-side subscription
	// immediately and cannot be undone (NMI delete_subscription once executed,
	// Solana).
	CancelModeDestructive CancelMode = "destructive"

	// CancelModeExternalPortal: we cannot cancel or resume from our system; the
	// user must use the rail's own consumer portal. No current rail (#696 moved
	// CCBill onto the DataLink SMS cancel); kept for the API vocabulary.
	CancelModeExternalPortal CancelMode = "external_portal"
)

// CredentialKey is one PSP secret slot on a rail.
// MerchantWritable=false marks operator-only secrets (e.g. the Solana signing
// key): merchant admins may neither write them nor see them in the redacted
// credential-status view.
type CredentialKey struct {
	Name             string
	MerchantWritable bool
}

// Descriptor declares one rail's capabilities and metadata (#669) — the facts
// that previously lived in scattered per-rail switches. Function fields carry
// per-rail behavior that is still a fact of the rail, not deployment wiring.
type Descriptor struct {
	Rail models.Rail

	// DisplayName is the subscriber-facing name (emails).
	DisplayName string

	// HasPSPs: the rail participates in the operator-declared
	// PSP catalog (openrails.psps).
	HasPSPs bool

	// HasRemoteCustomer: the rail exposes a PERSON-level remote customer
	// object worth materializing into rail_customer_accounts (#635) — Stripe cus_*
	// only. NMI's vault "customer" is an instrument container that OpenRails
	// deliberately mints PER CARD (#682), so it is not person-unique and does
	// not qualify; CCBill keys on subscription_id and Solana on the wallet
	// address — none of these is a customer.
	HasRemoteCustomer bool

	// SupportsChargeSavedMethod: invoice collection may charge a saved method
	// on this rail (prepaid auto-top-up #239, arrears settlement #241).
	SupportsChargeSavedMethod bool

	// SupportsCatalogTrial: the rail can honour a catalog first phase
	// (`trial:` — free or paid intro) declared on a price (or#896). Stripe
	// maps it onto subscription_data[trial_end]; CCBill's FlexForm carries the
	// terms and OpenRails validates the billed amount against them. NMI's
	// add_subscription and Solana's on-chain plan have no first-phase concept —
	// declaring a trial there is REFUSED at catalog push, never silently dropped.
	SupportsCatalogTrial bool

	// SupportsPaymentMethodCRUD: OpenRails owns first-party payment-instrument
	// CRUD on this rail (or#896). NMI only — its customer vault is the one
	// instrument store OpenRails writes. Stripe delegates to Checkout / the
	// Billing Portal, CCBill owns its own vault, Solana has no instrument.
	SupportsPaymentMethodCRUD bool

	// OpenRailsDrivenDunning: OpenRails owns the retry timing (models grace
	// access as explicit entitlement windows during dunning). NMI + Solana
	// recurring (#256/#257) — both charged by an OpenRails worker. Stripe
	// drives its own dunning and emits webhooks.
	OpenRailsDrivenDunning bool

	// RemoteDeleteOnTerminalCancel: a terminal cancellation must durably queue
	// deletion of the rail-side recurring schedule or the provider keeps
	// rebilling it (#344/#679). NMI only: Stripe/CCBill drive their own
	// lifecycle; Solana recurring is pulled by our cranker (stopped by the
	// local cancel cascade).
	RemoteDeleteOnTerminalCancel bool

	// AutoBilled reports whether the provider rebills this subscription on its
	// own side, so OpenRails must not manual-rebill or terminate it (#635), AS
	// CONSULTED BY THE DUNNING WORKER: CCBill always; NMI only when vault-less
	// (a vault-less recurring sub auto-charges on the remote subscription id).
	// Stripe is declared false here although it rebills itself — the dunning
	// worker never processes Stripe cohorts (OpenRailsDrivenDunning=false), and
	// the historical switch returned false; preserved deliberately (#669 note B).
	AutoBilled func(pm *models.PaymentMethod) bool

	// CancelMode classifies how a cancellation of a subscription on this rail
	// behaves. Takes the full subscription (never nil — callers guard) because
	// NMI is state-conditional: reversible only while its deferred delete is
	// still pending (issue 216). Never nil.
	CancelMode func(sub *models.Subscription, now time.Time) CancelMode

	// CancelPortalURL is the rail's external consumer portal for subscription
	// management, set only for CancelModeExternalPortal rails. "" = none.
	CancelPortalURL string

	// CredentialKeys are the rail's PSP secret slots, in display
	// order. Nil = the rail holds no PSP secrets.
	CredentialKeys []CredentialKey
}

func autoBilledNever(*models.PaymentMethod) bool  { return false }
func autoBilledAlways(*models.PaymentMethod) bool { return true }

func cancelReversible(*models.Subscription, time.Time) CancelMode  { return CancelModeReversible }
func cancelDestructive(*models.Subscription, time.Time) CancelMode { return CancelModeDestructive }

// nmiCancelMode: reversible only while the subscription is cancelled, its
// deferred delete_subscription has not yet executed (DeletionScheduledAt is
// cleared by the River finalizer), and the paid period is still in the future
// (issue 216). Once the delete fires or the period lapses it is destructive.
func nmiCancelMode(sub *models.Subscription, now time.Time) CancelMode {
	if sub.Status != models.StatusCancelled {
		return CancelModeDestructive
	}
	if sub.DeletionScheduledAt == nil || sub.DeletionScheduledAt.IsZero() {
		return CancelModeDestructive
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() || !sub.CurrentPeriodEndsAt.After(now) {
		return CancelModeDestructive
	}
	return CancelModeReversible
}

// nmiAutoBilled: reads the EXPLICIT rebill-driver mode (#682) — previously
// inferred from RailMethodRef emptiness, which made the identity field a
// behavior flag and blocked capturing billing ids for native vaults (#663).
// Migration 058 backfilled 'openrails' exactly where the old rule inferred it
// (legacy-imported methods carrying a billing id).
func nmiAutoBilled(pm *models.PaymentMethod) bool {
	return pm == nil || pm.RebillDriver != models.RebillDriverOpenRails
}

// descriptors is the compile-time-complete registry: UNKEYED struct literals,
// so adding a Descriptor field fails to compile until every rail declares it.
// Order is stable and load-bearing (reconcile iterates All()).
var descriptors = []Descriptor{
	{
		models.RailNMI,
		"Credit Card", // DisplayName
		true,          // HasPSPs
		false,         // HasRemoteCustomer (#682: vault ids are per-card instrument containers, not persons)
		true,          // SupportsChargeSavedMethod
		false,         // SupportsCatalogTrial (add_subscription has no first phase — or#896 refuses the declaration)
		true,          // SupportsPaymentMethodCRUD (the customer vault)
		true,          // OpenRailsDrivenDunning
		true,          // RemoteDeleteOnTerminalCancel (or NMI keeps retrying the schedule forever)
		nmiAutoBilled,
		nmiCancelMode,
		"", // CancelPortalURL
		// or#880: a custodian's private application key is NOT an NMI
		// credential. It belongs to the custodian account, not to whichever
		// gateway its proxy detokenizes into, and lives under
		// custodians/<kind>/<environment>/<account_id>/api_key.
		[]CredentialKey{{"security_key", true}, {"webhook_signing_secret", true}},
	},
	{
		models.RailCCBill,
		"Credit Card", // DisplayName
		true,          // HasPSPs
		false,         // HasRemoteCustomer (keys on subscription_id)
		false,         // SupportsChargeSavedMethod
		true,          // SupportsCatalogTrial (FlexForm terms; OpenRails validates the billed amount)
		false,         // SupportsPaymentMethodCRUD (CCBill owns the vault)
		false,         // OpenRailsDrivenDunning (CCBill retries itself)
		false,         // RemoteDeleteOnTerminalCancel (CCBill drives its own lifecycle)
		autoBilledAlways,
		cancelDestructive, // #696: DataLink SMS cancel — no resume API, access rides the paid runway
		"",                // CancelPortalURL (none since #696; cancels happen on OUR site)
		[]CredentialKey{{"salt", true}, {"datalink_username", true}, {"datalink_password", true}},
	},
	{
		models.RailStripe,
		"Stripe", // DisplayName
		true,     // HasPSPs
		true,     // HasRemoteCustomer (cus_*)
		true,     // SupportsChargeSavedMethod
		true,     // SupportsCatalogTrial (subscription_data[trial_end]; a PAID intro is refused separately)
		false,    // SupportsPaymentMethodCRUD (Checkout / Billing Portal own instrument mutation)
		false,    // OpenRailsDrivenDunning (Stripe dunning + webhooks)
		false,    // RemoteDeleteOnTerminalCancel (Stripe cancels its own schedule)
		autoBilledNever,
		cancelReversible, // cancel_at_period_end
		"",               // CancelPortalURL
		// webhook_signing_secret_previous (#856): the outgoing secret, retained
		// for the rollover overlap so events still queued on the superseded
		// endpoint keep verifying. Dropped when the last predecessor retires.
		[]CredentialKey{{"secret_key", true}, {"webhook_signing_secret", true}, {"webhook_signing_secret_thin", true}, {"webhook_signing_secret_previous", true}},
	},
	{
		models.RailSolana,
		"Solana", // DisplayName
		true,     // HasPSPs
		false,    // HasRemoteCustomer (keys on wallet address)
		false,    // SupportsChargeSavedMethod
		false,    // SupportsCatalogTrial (the on-chain plan has one period price — or#896 refuses the declaration)
		false,    // SupportsPaymentMethodCRUD (a wallet is not an instrument we hold)
		true,     // OpenRailsDrivenDunning (recurring pulled by our worker)
		false,    // RemoteDeleteOnTerminalCancel (local cancel cascade stops the cranker)
		autoBilledNever,
		cancelDestructive,
		"",                                      // CancelPortalURL
		[]CredentialKey{{"private_key", false}}, // operator-only signer
	},
	{
		models.RailPayPal,
		"PayPal", // DisplayName
		false,    // HasPSPs (no integration; display-only vestige)
		false,    // HasRemoteCustomer
		false,    // SupportsChargeSavedMethod
		false,    // SupportsCatalogTrial
		false,    // SupportsPaymentMethodCRUD
		false,    // OpenRailsDrivenDunning
		false,    // RemoteDeleteOnTerminalCancel
		autoBilledNever,
		cancelDestructive,
		"", // CancelPortalURL
		nil,
	},
}

var registry = func() map[models.Rail]Descriptor {
	m := make(map[models.Rail]Descriptor, len(descriptors))
	for _, d := range descriptors {
		if _, dup := m[d.Rail]; dup {
			panic(fmt.Sprintf("rails: duplicate descriptor for rail %q", d.Rail))
		}
		m[d.Rail] = d
	}
	return m
}()

// Lookup returns the descriptor for a rail. The rail value is normalized
// (lower-cased, trimmed) before matching; unknown rails return ok=false.
func Lookup(rail models.Rail) (Descriptor, bool) {
	d, ok := registry[models.Rail(normalize.Lower(string(rail)))]
	return d, ok
}

// All returns every descriptor in stable declaration order.
func All() []Descriptor {
	out := make([]Descriptor, len(descriptors))
	copy(out, descriptors)
	return out
}

// HasRemoteCustomer reports whether the rail exposes a card-independent remote
// customer object (#635). Unknown rails: false.
func HasRemoteCustomer(rail models.Rail) bool {
	d, ok := Lookup(rail)
	return ok && d.HasRemoteCustomer
}

// SupportsPSPs reports whether the rail participates in the
// PSP catalog. Unknown rails: false.
func SupportsPSPs(rail models.Rail) bool {
	d, ok := Lookup(rail)
	return ok && d.HasPSPs
}

// SupportsCatalogTrial reports whether a catalog `trial:` first phase can be
// executed on this rail (or#896). Unknown rails: false — a trial declared
// against something we cannot classify is refused, never dropped.
func SupportsCatalogTrial(rail models.Rail) bool {
	d, ok := Lookup(rail)
	return ok && d.SupportsCatalogTrial
}

// SupportsPaymentMethodCRUD reports whether OpenRails owns first-party
// payment-instrument CRUD on this rail (or#896). Unknown rails: false.
func SupportsPaymentMethodCRUD(rail models.Rail) bool {
	d, ok := Lookup(rail)
	return ok && d.SupportsPaymentMethodCRUD
}

// AutoBilled reports whether the provider rebills the subscription itself
// given its payment method (see Descriptor.AutoBilled). Unknown rails: false.
func AutoBilled(rail models.Rail, pm *models.PaymentMethod) bool {
	d, ok := Lookup(rail)
	return ok && d.AutoBilled(pm)
}

// RemoteDeleteOnTerminalCancel reports whether a terminal cancellation on this
// rail must durably queue deletion of the rail-side recurring schedule
// (#344/#679). Unknown rails: false.
func RemoteDeleteOnTerminalCancel(rail models.Rail) bool {
	d, ok := Lookup(rail)
	return ok && d.RemoteDeleteOnTerminalCancel
}

// CancelModeFor returns the cancellation capability for a subscription. Nil
// subscriptions and unknown rails yield the safe default: destructive.
func CancelModeFor(sub *models.Subscription, now time.Time) CancelMode {
	if sub == nil {
		return CancelModeDestructive
	}
	if d, ok := Lookup(sub.Rail); ok {
		return d.CancelMode(sub, now)
	}
	return CancelModeDestructive
}

// CancelPortalURL returns the rail's external consumer-portal URL ("" = none).
func CancelPortalURL(rail models.Rail) string {
	d, _ := Lookup(rail)
	return d.CancelPortalURL
}

// DisplayName returns the subscriber-facing rail name. Unknown non-empty rails
// fall back to the upper-cased value; empty stays empty (legacy email behavior).
func DisplayName(rail models.Rail) string {
	if d, ok := Lookup(rail); ok {
		return d.DisplayName
	}
	clean := strings.TrimSpace(string(rail))
	if clean == "" {
		return ""
	}
	return strings.ToUpper(clean)
}

// CredentialKeys returns the rail's full credential-key set (nil for none).
func CredentialKeys(rail models.Rail) []CredentialKey {
	d, ok := Lookup(rail)
	if !ok {
		return nil
	}
	return d.CredentialKeys
}

// MerchantCredentialKeyNames returns the merchant-writable credential key
// names in declaration order — the keys shown in the merchant-facing
// credential-status view. Nil when the rail has none.
func MerchantCredentialKeyNames(rail models.Rail) []string {
	var out []string
	for _, k := range CredentialKeys(rail) {
		if k.MerchantWritable {
			out = append(out, k.Name)
		}
	}
	return out
}

// CredentialKeyFor returns the rail's credential-key entry by name.
func CredentialKeyFor(rail models.Rail, key string) (CredentialKey, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, k := range CredentialKeys(rail) {
		if k.Name == key {
			return k, true
		}
	}
	return CredentialKey{}, false
}
