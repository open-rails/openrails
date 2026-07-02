package rails

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// CredentialKey is one provider-account secret slot on a rail.
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

	// HasProviderAccounts: the rail participates in the operator-declared
	// provider-account catalog (openrails.payment_provider_accounts).
	HasProviderAccounts bool

	// HasRemoteCustomer: the rail exposes a card-independent remote CUSTOMER
	// object worth materializing into rail_customers (#635) — Stripe cus_*,
	// NMI customer_vault_id. CCBill keys on subscription_id and Solana on the
	// wallet address; neither is a customer.
	HasRemoteCustomer bool

	// SupportsChargeSavedMethod: invoice collection may charge a saved method
	// on this rail (prepaid auto-top-up #239, arrears settlement #241).
	SupportsChargeSavedMethod bool

	// OpenRailsDrivenDunning: OpenRails owns the retry timing (models grace
	// access as explicit entitlement windows during dunning). NMI + Solana
	// recurring (#256/#257) — both charged by an OpenRails worker. Stripe
	// drives its own dunning and emits webhooks.
	OpenRailsDrivenDunning bool

	// RenewalGraceEligible: subscriptions get the pre-appended renewal grace
	// window (#368): NMI + Stripe. CCBill keeps its own retry-driven grace;
	// Solana is pull-based (no webhook silence to bridge).
	RenewalGraceEligible bool

	// AutoBilled reports whether the provider rebills this subscription on its
	// own side, so OpenRails must not manual-rebill or terminate it (#635), AS
	// CONSULTED BY THE DUNNING WORKER: CCBill always; NMI only when vault-less
	// (a vault-less recurring sub auto-charges on the remote subscription id).
	// Stripe is declared false here although it rebills itself — the dunning
	// worker never processes Stripe cohorts (OpenRailsDrivenDunning=false), and
	// the historical switch returned false; preserved deliberately (#669 note B).
	AutoBilled func(pm *models.PaymentMethod) bool

	// CredentialKeys are the rail's provider-account secret slots, in display
	// order. Nil = the rail holds no provider-account secrets.
	CredentialKeys []CredentialKey
}

func autoBilledNever(*models.PaymentMethod) bool  { return false }
func autoBilledAlways(*models.PaymentMethod) bool { return true }

// nmiAutoBilled: vault-less NMI recurring subs are provider-billed; a sub WITH
// a stored vault is our-rebill.
func nmiAutoBilled(pm *models.PaymentMethod) bool {
	return pm == nil || strings.TrimSpace(pm.RailMethodRef) == ""
}

// descriptors is the compile-time-complete registry: UNKEYED struct literals,
// so adding a Descriptor field fails to compile until every rail declares it.
// Order is stable and load-bearing (reconcile iterates All()).
var descriptors = []Descriptor{
	{
		models.RailNMI,
		"Credit Card", // DisplayName
		true,          // HasProviderAccounts
		true,          // HasRemoteCustomer (customer_vault_id)
		true,          // SupportsChargeSavedMethod
		true,          // OpenRailsDrivenDunning
		true,          // RenewalGraceEligible
		nmiAutoBilled,
		[]CredentialKey{{"security_key", true}, {"webhook_signing_secret", true}},
	},
	{
		models.RailCCBill,
		"Credit Card", // DisplayName
		true,          // HasProviderAccounts
		false,         // HasRemoteCustomer (keys on subscription_id)
		false,         // SupportsChargeSavedMethod
		false,         // OpenRailsDrivenDunning (CCBill retries itself)
		false,         // RenewalGraceEligible (grace from CCBill nextRetryDate)
		autoBilledAlways,
		[]CredentialKey{{"salt", true}, {"datalink_username", true}, {"datalink_password", true}},
	},
	{
		models.RailStripe,
		"Stripe", // DisplayName
		true,     // HasProviderAccounts
		true,     // HasRemoteCustomer (cus_*)
		true,     // SupportsChargeSavedMethod
		false,    // OpenRailsDrivenDunning (Stripe dunning + webhooks)
		true,     // RenewalGraceEligible
		autoBilledNever,
		[]CredentialKey{{"secret_key", true}, {"webhook_signing_secret", true}, {"webhook_signing_secret_thin", true}},
	},
	{
		models.RailSolana,
		"Solana", // DisplayName
		true,     // HasProviderAccounts
		false,    // HasRemoteCustomer (keys on wallet address)
		false,    // SupportsChargeSavedMethod
		true,     // OpenRailsDrivenDunning (recurring pulled by our worker)
		false,    // RenewalGraceEligible (pull-based)
		autoBilledNever,
		[]CredentialKey{{"private_key", false}}, // operator-only signer
	},
	{
		models.RailPayPal,
		"PayPal", // DisplayName
		false,    // HasProviderAccounts (no integration; display-only vestige)
		false,    // HasRemoteCustomer
		false,    // SupportsChargeSavedMethod
		false,    // OpenRailsDrivenDunning
		false,    // RenewalGraceEligible
		autoBilledNever,
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

// SupportsProviderAccounts reports whether the rail participates in the
// provider-account catalog. Unknown rails: false.
func SupportsProviderAccounts(rail models.Rail) bool {
	d, ok := Lookup(rail)
	return ok && d.HasProviderAccounts
}

// AutoBilled reports whether the provider rebills the subscription itself
// given its payment method (see Descriptor.AutoBilled). Unknown rails: false.
func AutoBilled(rail models.Rail, pm *models.PaymentMethod) bool {
	d, ok := Lookup(rail)
	return ok && d.AutoBilled(pm)
}

// RenewalGraceEligible reports whether the rail's subscriptions get the
// pre-appended renewal grace window (#368). Unknown rails: false.
func RenewalGraceEligible(rail models.Rail) bool {
	d, ok := Lookup(rail)
	return ok && d.RenewalGraceEligible
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
