package config

import (
	"fmt"

	"github.com/goccy/go-yaml"
)

// MerchantDeclaration declares one merchant: the embedded engine's merchant
// (Config.Merchant) or one entry of a standalone merchant manifest.
type MerchantDeclaration struct {
	// Slug is the merchant's name. A manifest keys merchants by slug instead.
	Slug        string `yaml:"-" koanf:"-"`
	DisplayName string `yaml:"display_name" koanf:"display_name"`
	// APIHost is the merchant's canonical #734 API host (e.g. "api.myapp.example"):
	// the Host-header value public routes resolve this merchant from (#850).
	// Globally unique across active merchants. Omitted leaves the stored value
	// untouched (it can also be assigned via PUT /v1/merchant/api-host).
	APIHost string                `yaml:"api_host,omitempty" koanf:"api_host"`
	Profile MerchantProfileConfig `yaml:"profile,omitempty" koanf:"profile"`
	// Invoice is the merchant's billing/collection policy (#643/#646): when/how the
	// accrued balance is invoiced. Omitted leaves all values at the service default;
	// an omitted field within the block leaves that field as-is.
	Invoice *InvoiceConfig `yaml:"invoice,omitempty" koanf:"invoice"`
	// DelegatedInvokerWastedSpendWindows are merchant-wide abuse cutoffs for
	// delegated invokers (#646): per-window spend ceilings on wasted (failed/abused)
	// generation. Empty leaves the service-default windows (burst 15m/$5, sustained 5h/$20).
	DelegatedInvokerWastedSpendWindows []BudgetWindowConfig `yaml:"delegated_invoker_wasted_spend_windows,omitempty" koanf:"delegated_invoker_wasted_spend_windows"`
	// PSPs is the operator-declared PSP catalog: the merchant's payment
	// service providers, keyed by PSP key (e.g. mobius), one rail each.
	// merchants.<slug>. nothing else it could mean). ONE word everywhere: the
	// DB table (billing.psps) and the merchant-secret name prefix (`psps/…`)
	// speak the same vocabulary as this key.
	PSPs map[string]PSPConfig `yaml:"psps,omitempty" koanf:"psps"`
	// Custodians is the operator-declared CUSTODIAN catalog (or#880), keyed by
	// custodian key, one kind each — the exact shape of `psps:` one axis over.
	// A custodian holds the card; a PSP charges it. Declared ONCE here and
	// referenced by every PSP whose gateway those cards are charged through,
	// so a merchant running a live and a sandbox gateway against the same
	// vault has one tenant id and one application key, not two copies that
	// drift.
	Custodians map[string]CustodianConfig `yaml:"custodians,omitempty" koanf:"custodians"`
	// CheckoutRouting (or#288) is the merchant's processor preference policy:
	// ordered rules, first match wins, each naming a ranked PSP list. Omitted
	// leaves the stored policy untouched; declared REPLACES it whole (an
	// ordered list has no meaningful per-element merge).
	CheckoutRouting []CheckoutRoutingRuleConfig `yaml:"checkout_routing,omitempty" koanf:"checkout_routing"`
	// BillingPolicies (or#897) are the merchant's named billing policies, keyed by
	// name. Each declares WHICH quantity is capped; BillingPolicyBindings decides
	// who gets which. Validated by the SAME normalizer the config API runs, so a
	// manifest that boots cannot declare a policy the API would refuse.
	BillingPolicies map[string]BillingPolicyConfig `yaml:"billing_policies,omitempty" koanf:"billing_policies"`
	// BillingPolicyBindings (or#897) point DECLARATIVE rungs at policy names:
	// `tier` for a trust tier, omitted for the merchant default. Per-customer
	// binding is the mode-2 API's runtime lever, not manifest truth.
	BillingPolicyBindings []BillingPolicyBindingConfig `yaml:"billing_policy_bindings,omitempty" koanf:"billing_policy_bindings"`
}

// BillingPolicyConfig is one manifest billing policy (or#897). Amounts are in
// the currency's micros; window durations are Go durations ("720h").
type BillingPolicyConfig struct {
	// Kind: outstanding_cap | window_spend_cap | accrual_rate_cap.
	Kind string `yaml:"kind" koanf:"kind"`
	// OutstandingCap (micros, kind=outstanding_cap) is the credit line on unpaid
	// arrears. Omitted defers to the payer's own arrears credit limit.
	OutstandingCap int64 `yaml:"outstanding_cap,omitempty" koanf:"outstanding_cap"`
	// SpendWindows (kind=window_spend_cap) are the rolling NEW-spend ceilings.
	SpendWindows []BudgetWindowConfig `yaml:"spend_windows,omitempty" koanf:"spend_windows"`
	// AccrualRateCapPerHour (micros per HOUR, kind=accrual_rate_cap) is the cloud
	// quota. AccrualRateWindow is the measurement lookback as a Go duration
	// ("1h"); omitted means one hour. It changes the smoothing, never the unit.
	AccrualRateCapPerHour int64  `yaml:"accrual_rate_cap_per_hour,omitempty" koanf:"accrual_rate_cap_per_hour"`
	AccrualRateWindow     string `yaml:"accrual_rate_window,omitempty" koanf:"accrual_rate_window"`
	// BadSpendWindows are the #497 per-PAYER wasted-spend grace windows.
	BadSpendWindows []BudgetWindowConfig `yaml:"bad_spend_windows,omitempty" koanf:"bad_spend_windows"`
	// CollectionThreshold (micros) is when payers bound here are invoiced;
	// DelinquencyGraceDays / DelinquencyAmountFloor are their delinquency policy.
	// Each overrides the merchant-wide `invoice:` block for those payers only.
	CollectionThreshold *int64 `yaml:"collection_threshold,omitempty" koanf:"collection_threshold"`
	// CollectionCycleBoundary is declarable and REFUSED: statement periods must
	// tile a payer's lifetime, and rebinding is a live lever, so the boundary
	// stays merchant-wide. Declaring it here fails with that reason.
	CollectionCycleBoundary string `yaml:"collection_cycle_boundary,omitempty" koanf:"collection_cycle_boundary"`
	DelinquencyGraceDays    *int   `yaml:"delinquency_grace_days,omitempty" koanf:"delinquency_grace_days"`
	DelinquencyAmountFloor  *int64 `yaml:"delinquency_amount_floor,omitempty" koanf:"delinquency_amount_floor"`
	PolicyCurrency          string `yaml:"policy_currency,omitempty" koanf:"policy_currency"`
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
	Policy string `yaml:"policy" koanf:"policy"`
	Tier   string `yaml:"tier,omitempty" koanf:"tier"`
}

// CheckoutRoutingRuleConfig is one manifest routing rule. Prefer speaks the
// checkout selector vocabulary (#848): PSP keys, or a rail kind where exactly
// one PSP is armed on it.
type CheckoutRoutingRuleConfig struct {
	Match  CheckoutRoutingMatchConfig `yaml:"match,omitempty" koanf:"match"`
	Prefer []string                   `yaml:"prefer" koanf:"prefer"`
}

// CheckoutRoutingMatchConfig is a rule's condition; every set field must match,
// and an all-empty match is the catch-all (which must be the last rule).
type CheckoutRoutingMatchConfig struct {
	Currency string `yaml:"currency,omitempty" koanf:"currency"`
	Product  string `yaml:"product,omitempty" koanf:"product"`
	Price    string `yaml:"price,omitempty" koanf:"price"`
	Mode     string `yaml:"mode,omitempty" koanf:"mode"`
	Country  string `yaml:"country,omitempty" koanf:"country"`
}

// InvoiceConfig is the merchant invoice/collection policy block, mirroring
// the merchant_configurations invoice fields. Amounts are in the currency's micros.
type InvoiceConfig struct {
	// CollectionThreshold: invoice an arrears customer once their accrued balance
	// reaches this (micros). Default 50_000_000 ($50).
	CollectionThreshold *int64 `yaml:"collection_threshold,omitempty" koanf:"collection_threshold"`
	// MonthlyFloor: don't bother collecting below this (micros). Default 1_000_000 ($1).
	MonthlyFloor *int64 `yaml:"monthly_floor,omitempty" koanf:"monthly_floor"`
	// BillingPeriodBoundary: calendar_month | anniversary | fixed_interval.
	// Default fixed_interval (rolling 30d). calendar_month resets on the 1st.
	BillingPeriodBoundary string `yaml:"billing_period_boundary,omitempty" koanf:"billing_period_boundary"`
	// DelinquencyGraceDays (or#878): days past an invoice's due date before the
	// payer is DELINQUENT — new spend refused and the host signalled to shut off
	// whatever it runs. Business policy, so it is the merchant's. Default 14;
	// 0 means delinquent as soon as it is overdue.
	DelinquencyGraceDays *int `yaml:"delinquency_grace_days,omitempty" koanf:"delinquency_grace_days"`
	// DelinquencyAmountFloor (micros): the smallest overdue balance that can
	// escalate. Unset DERIVES from monthly_floor — a debt too small to bother
	// collecting is too small to cut anyone off for.
	DelinquencyAmountFloor *int64 `yaml:"delinquency_amount_floor,omitempty" koanf:"delinquency_amount_floor"`
}

// BudgetWindowConfig is one delegated-invoker wasted-spend window. Window is a
// Go duration ("15m", "5h"); Limit is the per-window ceiling in the currency's micros.
type BudgetWindowConfig struct {
	Key      string `yaml:"key" koanf:"key"`
	Window   string `yaml:"window" koanf:"window"`
	Limit    int64  `yaml:"limit" koanf:"limit"`
	Currency string `yaml:"currency,omitempty" koanf:"currency"`
}

type MerchantProfileConfig struct {
	DisplayName string `yaml:"display_name,omitempty" koanf:"display_name"`
	LogoURL     string `yaml:"logo_url,omitempty" koanf:"logo_url"`
	FromEmail   string `yaml:"from_email,omitempty" koanf:"from_email"`
	SupportURL  string `yaml:"support_url,omitempty" koanf:"support_url"`
	SignupURL   string `yaml:"signup_url,omitempty" koanf:"signup_url"`
}

type PSPConfig map[string]ProviderRailAccountConfig

// CustodianConfig is one declared custodian's kind block: exactly one entry,
// keyed by the vendor kind (basis_theory), mirroring PSPConfig's rail key.
type CustodianConfig map[string]CustodianAccountConfig

// CustodianAccountConfig is one merchant-owned account with a custodian.
type CustodianAccountConfig struct {
	// AccountID is the custodian-native tenant identity (Basis Theory: the
	// tenant id). Operator-declared — there is no runtime whoami (#592).
	AccountID string `yaml:"account_id,omitempty" koanf:"account_id"`
	// Archived drains the custodian: instruments it holds stay chargeable, no
	// new arrangement may reference it.
	Archived bool `yaml:"archived,omitempty" koanf:"archived"`
	// Settings are the declared NON-secret knobs, validated against the kind's
	// registry (internal/custodians): public_api_key, network_tokens.
	Settings map[string]any `yaml:"settings,omitempty" koanf:"settings"`
	// Secrets are the kind's credential slots (Basis Theory: api_key, the
	// private application key). Stored under
	// custodians/<kind>/<environment>/<account_id>/<key>.
	Secrets map[string]string `yaml:"secrets,omitempty" koanf:"secrets"`
}

type ProviderRailAccountConfig struct {
	// LegacyEnvironment keeps the retired `environment:` key parseable ONLY so a
	// manifest that still declares it fails loudly (#882). The environment is
	// DERIVED from test_mode — it never was anything else, since a declared value
	// that disagreed refused to boot and one that agreed was a no-op.
	LegacyEnvironment string `yaml:"environment,omitempty" koanf:"environment"`
	AccountID         string `yaml:"account_id,omitempty" koanf:"account_id"`
	Archived          bool   `yaml:"archived,omitempty" koanf:"archived"`
	// Custodian REFERENCES a declared custodian by key (or#880):
	// merchants.<slug>.custodians.<key>. "" = this PSP holds its own
	// instruments (Stripe pm_, NMI customer vault), which is the common case
	// and needs no configuration. A key with no matching custodian entry is a
	// hard error, never an unarmed default.
	Custodian string            `yaml:"custodian,omitempty" koanf:"custodian"`
	Signer    *PSPSignerConfig  `yaml:"signer,omitempty" koanf:"signer"`
	Secrets   map[string]string `yaml:"secrets,omitempty" koanf:"secrets"`
	// Settings are per-account NON-SECRET runtime knobs, stored on the
	// psps row (NMI: tokenization_key/tokenization_url;
	// Solana: rpc_provider, rpc_api_key, tokens,
	// recipient_wallet — see config.SolanaAccountSettings, #711).
	Settings map[string]any `yaml:"settings,omitempty" koanf:"settings"`
}

type PSPSignerConfig struct {
	Mode string `yaml:"mode,omitempty" koanf:"mode"`
	Key  string `yaml:"key,omitempty" koanf:"key"`
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
