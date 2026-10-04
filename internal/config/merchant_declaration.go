package config

import (
	"fmt"

	"github.com/goccy/go-yaml"
)

// MerchantDeclaration declares one merchant: the embedded engine's merchant
// (Config.Merchant) or one entry of a standalone merchant manifest.
type MerchantDeclaration struct {
	// Slug is the merchant's name. A manifest keys merchants by slug instead.
	Slug        string `yaml:"-"`
	DisplayName string `yaml:"display_name"`
	// APIHost is the merchant's canonical API host (e.g. "api.myapp.example"):
	// the Host-header value public routes resolve this merchant from.
	// Globally unique across active merchants. Omitted leaves the stored value
	// untouched (it can also be assigned via PUT /v1/merchant/api-host).
	APIHost string                `yaml:"api_host,omitempty"`
	Profile MerchantProfileConfig `yaml:"profile,omitempty"`
	// Invoice is the merchant's billing/collection policy: when/how the
	// accrued balance is invoiced. Omitted leaves all values at the service default;
	// an omitted field within the block leaves that field as-is.
	Invoice *InvoiceConfig `yaml:"invoice,omitempty"`
	// DelegatedInvokerWastedSpendWindows are merchant-wide abuse cutoffs for
	// delegated invokers: per-window spend ceilings on wasted (failed/abused)
	// generation. Empty leaves the service-default windows (burst 15m/$5, sustained 5h/$20).
	DelegatedInvokerWastedSpendWindows []BudgetWindowConfig `yaml:"delegated_invoker_wasted_spend_windows,omitempty"`
	// PSPs is the operator-declared PSP catalog: the merchant's payment
	// service providers, keyed by PSP key (e.g. mobius), one rail each.
	// merchants.<slug>. nothing else it could mean). ONE word everywhere: the
	// DB table (billing.psps) and the merchant-secret name prefix (`psps/…`)
	// speak the same vocabulary as this key.
	PSPs map[string]PSPConfig `yaml:"psps,omitempty"`
	// Custodians is the operator-declared custodian catalog, keyed by
	// custodian key, one kind each — the exact shape of `psps:` one axis over.
	// A custodian holds the card; a PSP charges it. Declared ONCE here and
	// referenced by every PSP whose gateway those cards are charged through,
	// so a merchant running a live and a sandbox gateway against the same
	// vault has one tenant id and one application key, not two copies that
	// drift.
	Custodians map[string]CustodianConfig `yaml:"custodians,omitempty"`
	// CheckoutRouting is the merchant's processor preference policy:
	// ordered rules, first match wins, each naming a ranked PSP list. Omitted
	// leaves the stored policy untouched; declared REPLACES it whole (an
	// ordered list has no meaningful per-element merge).
	CheckoutRouting []CheckoutRoutingRuleConfig `yaml:"checkout_routing,omitempty"`
	// BillingPolicies are the merchant's named billing policies, keyed by
	// name. Each declares WHICH quantity is capped; BillingPolicyBindings decides
	// who gets which. Validated by the SAME normalizer the config API runs, so a
	// manifest that boots cannot declare a policy the API would refuse.
	BillingPolicies map[string]BillingPolicyConfig `yaml:"billing_policies,omitempty"`
	// BillingPolicyBindings point declarative rungs at policy names:
	// `tier` for a trust tier, omitted for the merchant default. Per-customer
	// binding is the mode-2 API's runtime lever, not manifest truth.
	BillingPolicyBindings []BillingPolicyBindingConfig `yaml:"billing_policy_bindings,omitempty"`
}

// BillingPolicyConfig is one declared billing policy. Amounts are in
// the currency's micros; window durations are Go durations ("720h").
type BillingPolicyConfig struct {
	// Kind: outstanding_cap | window_spend_cap | accrual_rate_cap.
	Kind string `yaml:"kind"`
	// OutstandingCap (micros, kind=outstanding_cap) is the credit line on unpaid
	// arrears. Omitted defers to the payer's own arrears credit limit.
	OutstandingCap int64 `yaml:"outstanding_cap,omitempty"`
	// SpendWindows (kind=window_spend_cap) are the rolling NEW-spend ceilings.
	SpendWindows []BudgetWindowConfig `yaml:"spend_windows,omitempty"`
	// AccrualRateCapPerHour (micros per HOUR, kind=accrual_rate_cap) is the cloud
	// quota. AccrualRateWindow is the measurement lookback as a Go duration
	// ("1h"); omitted means one hour. It changes the smoothing, never the unit.
	AccrualRateCapPerHour int64  `yaml:"accrual_rate_cap_per_hour,omitempty"`
	AccrualRateWindow     string `yaml:"accrual_rate_window,omitempty"`
	// BadSpendWindows are the per-payer wasted-spend grace windows.
	BadSpendWindows []BudgetWindowConfig `yaml:"bad_spend_windows,omitempty"`
	// CollectionThreshold (micros) is when payers bound here are invoiced;
	// DelinquencyGraceDays / DelinquencyAmountFloor are their delinquency policy.
	// Each overrides the merchant-wide `invoice:` block for those payers only.
	CollectionThreshold *int64 `yaml:"collection_threshold,omitempty"`
	// CollectionCycleBoundary is declarable and REFUSED: statement periods must
	// tile a payer's lifetime, and rebinding is a live lever, so the boundary
	// stays merchant-wide. Declaring it here fails with that reason.
	CollectionCycleBoundary string `yaml:"collection_cycle_boundary,omitempty"`
	DelinquencyGraceDays    *int   `yaml:"delinquency_grace_days,omitempty"`
	DelinquencyAmountFloor  *int64 `yaml:"delinquency_amount_floor,omitempty"`
	PolicyCurrency          string `yaml:"policy_currency,omitempty"`
}

// BillingPolicyBindingConfig binds one DECLARATIVE rung to a policy name: a
// trust tier, or the merchant default when `tier` is omitted.
//
// There is deliberately no per-customer rung here. Binding a policy to one
// payer is the merchant's RUNTIME lever (mode-2 API), not declared truth: in
// mode 1 the YAML is the whole configuration and changing it means a reboot,
// which is the wrong shape for per-customer segmentation. It would also put
// customer identifiers into a committed manifest.
type BillingPolicyBindingConfig struct {
	Policy string `yaml:"policy"`
	Tier   string `yaml:"tier,omitempty"`
}

// CheckoutRoutingRuleConfig is one manifest routing rule. Prefer speaks the
// checkout selector vocabulary: PSP keys, or a rail kind where exactly
// one PSP is armed on it.
type CheckoutRoutingRuleConfig struct {
	Match  CheckoutRoutingMatchConfig `yaml:"match,omitempty"`
	Prefer []string                   `yaml:"prefer"`
}

// CheckoutRoutingMatchConfig is a rule's condition; every set field must match,
// and an all-empty match is the catch-all (which must be the last rule).
type CheckoutRoutingMatchConfig struct {
	Currency string `yaml:"currency,omitempty"`
	Product  string `yaml:"product,omitempty"`
	Price    string `yaml:"price,omitempty"`
	Mode     string `yaml:"mode,omitempty"`
	Country  string `yaml:"country,omitempty"`
}

// InvoiceConfig is the merchant invoice/collection policy block, mirroring
// the merchant_configurations invoice fields. Amounts are in the currency's micros.
type InvoiceConfig struct {
	// CollectionThreshold: invoice an arrears customer once their accrued balance
	// reaches this (micros). Default 50_000_000 ($50).
	CollectionThreshold *int64 `yaml:"collection_threshold,omitempty"`
	// MonthlyFloor: don't bother collecting below this (micros). Default 1_000_000 ($1).
	MonthlyFloor *int64 `yaml:"monthly_floor,omitempty"`
	// BillingPeriodBoundary: calendar_month | anniversary | fixed_interval.
	// Default fixed_interval (rolling 30d). calendar_month resets on the 1st.
	BillingPeriodBoundary string `yaml:"billing_period_boundary,omitempty"`
	// DelinquencyGraceDays is the days past an invoice's due date before the
	// payer is DELINQUENT — new spend refused and the host signalled to shut off
	// whatever it runs. Business policy, so it is the merchant's. Default 14;
	// 0 means delinquent as soon as it is overdue.
	DelinquencyGraceDays *int `yaml:"delinquency_grace_days,omitempty"`
	// DelinquencyAmountFloor (micros): the smallest overdue balance that can
	// escalate. Unset DERIVES from monthly_floor — a debt too small to bother
	// collecting is too small to cut anyone off for.
	DelinquencyAmountFloor *int64 `yaml:"delinquency_amount_floor,omitempty"`
}

// BudgetWindowConfig is one delegated-invoker wasted-spend window. Window is a
// Go duration ("15m", "5h"); Limit is the per-window ceiling in the currency's micros.
type BudgetWindowConfig struct {
	Key      string `yaml:"key"`
	Window   string `yaml:"window"`
	Limit    int64  `yaml:"limit"`
	Currency string `yaml:"currency,omitempty"`
}

// MerchantProfileConfig is the merchant's public face: the name, logo and
// links shown to customers and on billing email.
type MerchantProfileConfig struct {
	DisplayName string `yaml:"display_name,omitempty"`
	LogoURL     string `yaml:"logo_url,omitempty"`
	FromEmail   string `yaml:"from_email,omitempty"`
	SupportURL  string `yaml:"support_url,omitempty"`
	SignupURL   string `yaml:"signup_url,omitempty"`
}

// PSPConfig is one declared PSP: exactly one entry, keyed by its rail (nmi,
// ccbill, stripe, solana).
type PSPConfig map[string]ProviderRailAccountConfig

// CustodianConfig is one declared custodian's kind block: exactly one entry,
// keyed by the vendor kind (basis_theory), mirroring PSPConfig's rail key.
type CustodianConfig map[string]CustodianAccountConfig

// CustodianAccountConfig is one merchant-owned account with a custodian.
type CustodianAccountConfig struct {
	// AccountID is the custodian-native tenant identity (Basis Theory: the
	// tenant id). Operator-declared: there is no runtime lookup.
	AccountID string `yaml:"account_id,omitempty"`
	// Archived drains the custodian: instruments it holds stay chargeable, no
	// new arrangement may reference it.
	Archived bool `yaml:"archived,omitempty"`
	// Settings are the declared NON-secret knobs, validated against the kind's
	// registry (internal/custodians): public_api_key, network_tokens.
	Settings map[string]any `yaml:"settings,omitempty"`
	// Secrets are the kind's credential slots (Basis Theory: api_key, the
	// private application key). Stored under
	// custodians/<kind>/<environment>/<account_id>/<key>.
	Secrets map[string]string `yaml:"secrets,omitempty"`
}

// ProviderRailAccountConfig is a PSP's account on its rail.
type ProviderRailAccountConfig struct {
	// LegacyEnvironment keeps the retired `environment:` key parseable ONLY so a
	// manifest that still declares it fails loudly. The environment is
	// DERIVED from test_mode — it never was anything else, since a declared value
	// that disagreed refused to boot and one that agreed was a no-op.
	LegacyEnvironment string `yaml:"environment,omitempty"`
	AccountID         string `yaml:"account_id,omitempty"`
	Archived          bool   `yaml:"archived,omitempty"`
	// Custodian references a declared custodian by key:
	// merchants.<slug>.custodians.<key>. "" = this PSP holds its own
	// instruments (Stripe pm_, NMI customer vault), which is the common case
	// and needs no configuration. A key with no matching custodian entry is a
	// hard error, never an unarmed default.
	Custodian string            `yaml:"custodian,omitempty"`
	Signer    *PSPSignerConfig  `yaml:"signer,omitempty"`
	Secrets   map[string]string `yaml:"secrets,omitempty"`
	// Settings are per-account NON-SECRET runtime knobs, stored on the
	// psps row (NMI: tokenization_key/tokenization_url;
	// Solana: rpc_provider, rpc_api_key, tokens,
	// recipient_wallet).
	Settings map[string]any `yaml:"settings,omitempty"`
}

// PSPSignerConfig selects how a Solana PSP signs: Mode is "local_keypair" or
// "vault_transit", and Key names the Vault Transit key.
type PSPSignerConfig struct {
	Mode string `yaml:"mode,omitempty"`
	Key  string `yaml:"key,omitempty"`
}

// ParseMerchantDeclaration parses one merchant YAML document. Unknown fields
// are refused, so a typo fails loudly instead of declaring a merchant with no
// PSPs; the slug comes from the caller, not the document.
func ParseMerchantDeclaration(raw []byte) (MerchantDeclaration, error) {
	var probe map[string]any
	if yaml.Unmarshal(raw, &probe) == nil {
		for _, old := range []string{"rail_merchant_accounts", "provider_accounts"} {
			if _, ok := probe[old]; ok {
				return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %s was renamed to psps", old)
			}
		}
	}
	var m MerchantDeclaration
	if err := yaml.UnmarshalWithOptions(raw, &m, yaml.DisallowUnknownField()); err != nil {
		return MerchantDeclaration{}, fmt.Errorf("parse merchant declaration: %w", err)
	}
	return m, nil
}
