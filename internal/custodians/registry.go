// Package custodians is the registry of custodian KINDS (or#880 phase 3).
//
// A custodian holds the card; a rail charges it. The two axes are orthogonal —
// see docs/payment-method-custody.md. Everything a kind needs in order to be
// declared, validated, armed and driven from a browser is DATA in this file:
// its secret slots, its declared settings, which rails' charge paths can accept
// its instruments, and how a checkout page tokenizes against it.
//
// The point of the registry is that adding a second vendor (HyperSwitch,
// JusPay, Spreedly) is a Descriptor plus an implementation of the seams it
// names — never a new branch in the config validator, the manifest reconciler,
// the browser-config projection and the webhook router.
package custodians

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

// Browser flows a custodian can require of a checkout page. Same vocabulary as
// a rail's flow (internal/merchants): custody changes WHO the browser
// tokenizes against, not the shape of the interaction.
const FlowTokenize = "tokenize"

// ValueKind types a declared setting so one validator can parse every kind.
type ValueKind string

const (
	ValueString ValueKind = "string"
	ValueBool   ValueKind = "bool"
)

// SecretKey is one slot in the merchant secret store, addressed as
// `custodians/<kind>/<environment>/<account_id>/<name>`.
type SecretKey struct {
	Name string
	// Required: the custodian cannot arm without it, so a missing value is a
	// fail-closed error and never a silent downgrade to "no custody".
	Required bool
	// MerchantWritable mirrors the rail credential registry (#669): false marks
	// operator-only slots a merchant admin may neither write nor read back.
	MerchantWritable bool
}

// SettingKey is one declared NON-secret knob on a custodian entry.
type SettingKey struct {
	Name     string
	Kind     ValueKind
	Required bool
	// Public: safe to serve to an unauthenticated browser. This is the
	// whitelist the public checkout-config projection reads — a setting not
	// marked here never leaves the server.
	Public bool
	// PublicField is the wire name in the public checkout config (Public only).
	PublicField string
}

// Descriptor declares one custodian kind.
type Descriptor struct {
	// Kind is the declared value: the manifest's `custodians.<key>.<kind>`
	// block name, the custodians.kind column, and payment_methods.custodian.
	Kind string
	// DisplayName is the operator-facing name.
	DisplayName string
	// ProxyRails are the rails whose charge path can present an instrument
	// this custodian holds. A PSP on any other rail may not reference it —
	// the arrangement would have no way to charge.
	ProxyRails []models.Rail
	Secrets    []SecretKey
	Settings   []SettingKey
	// WebhookSource is the event source this custodian's own webhooks arrive
	// as. "" = the custodian sends none.
	WebhookSource models.EventSource
	// BrowserFlow is how a checkout page must drive it. "" = the browser never
	// talks to this custodian directly.
	BrowserFlow string
}

// Basis Theory setting/secret names. They live INSIDE the custodian's own
// block, so they carry no `custodian_` prefix — that prefix existed only while
// custody was smuggled into a PSP's settings map (or#879).
const (
	SettingPublicAPIKey  = "public_api_key"
	SettingNetworkTokens = "network_tokens"
	SecretAPIKey         = "api_key"
)

var registry = map[string]Descriptor{
	models.CustodianBasisTheory: {
		Kind:        models.CustodianBasisTheory,
		DisplayName: "Basis Theory",
		// NMI is the one rail with a detokenizing proxy charge path today.
		ProxyRails: []models.Rail{models.RailNMI},
		Secrets: []SecretKey{
			// The PRIVATE application key. It authorizes detokenization —
			// never a charge on its own, which still needs the rail's own
			// gateway credentials.
			{Name: SecretAPIKey, Required: true, MerchantWritable: true},
		},
		Settings: []SettingKey{
			// The PUBLIC application key the checkout page tokenizes with.
			{Name: SettingPublicAPIKey, Kind: ValueString, Required: true, Public: true, PublicField: "public_api_key"},
			// Network-token provisioning at instrument creation (the $-add-on;
			// never load-bearing — a provisioning failure warns and the
			// instrument stays pan_proxy).
			{Name: SettingNetworkTokens, Kind: ValueBool},
		},
		WebhookSource: models.EventSourceBasisTheory,
		BrowserFlow:   FlowTokenize,
	},
}

// Get returns the descriptor for a declared kind.
func Get(kind string) (Descriptor, bool) {
	d, ok := registry[Normalize(kind)]
	return d, ok
}

// Require returns the descriptor or a loud error naming the known kinds (#651
// — an unknown kind never silently degrades to "no custody").
func Require(kind string) (Descriptor, error) {
	d, ok := Get(kind)
	if !ok {
		return Descriptor{}, fmt.Errorf("unknown custodian kind %q (known: %s)", kind, strings.Join(Kinds(), ", "))
	}
	return d, nil
}

// Kinds lists the declared third-party custodian kinds in stable order.
// models.CustodianPSP is NOT one of them: "the PSP holds its own instruments"
// is the absence of a custodian, and needs no credentials of its own.
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for kind := range registry {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// Normalize canonicalizes a declared kind.
func Normalize(kind string) string { return strings.ToLower(strings.TrimSpace(kind)) }

// SupportsRail reports whether this custodian's instruments can be charged
// through rail.
func (d Descriptor) SupportsRail(rail models.Rail) bool {
	want := models.Rail(strings.ToLower(strings.TrimSpace(string(rail))))
	for _, r := range d.ProxyRails {
		if r == want {
			return true
		}
	}
	return false
}

// RailNames renders ProxyRails for error messages.
func (d Descriptor) RailNames() string {
	names := make([]string, 0, len(d.ProxyRails))
	for _, r := range d.ProxyRails {
		names = append(names, string(r))
	}
	return strings.Join(names, ", ")
}

// Secret returns the named secret slot.
func (d Descriptor) Secret(name string) (SecretKey, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, s := range d.Secrets {
		if s.Name == name {
			return s, true
		}
	}
	return SecretKey{}, false
}

// Setting returns the named setting slot.
func (d Descriptor) Setting(name string) (SettingKey, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, s := range d.Settings {
		if s.Name == name {
			return s, true
		}
	}
	return SettingKey{}, false
}

// SettingNames lists declared setting names in declaration order.
func (d Descriptor) SettingNames() []string {
	out := make([]string, 0, len(d.Settings))
	for _, s := range d.Settings {
		out = append(out, s.Name)
	}
	return out
}

// SecretNames lists declared secret names in declaration order.
func (d Descriptor) SecretNames() []string {
	out := make([]string, 0, len(d.Secrets))
	for _, s := range d.Secrets {
		out = append(out, s.Name)
	}
	return out
}
