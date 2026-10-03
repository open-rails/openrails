package openrails

import (
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/cache"
	"github.com/open-rails/openrails/internal/config"
)

// The Config and Deps family. These are the only aliases in this package: one
// definition in internal/config (and the auth hook types in
// internal/billingauth), named here for hosts.

type (
	// Config is the engine's plain-data configuration. TestMode and
	// ProviderWriteMode are required.
	Config = config.Config
	// Deps is everything the engine reaches outside its process: connections,
	// credentials and the host's hooks.
	Deps = config.Deps

	CredentialPosture      = config.CredentialPosture
	RiverOwnership         = config.RiverOwnership
	HTTPConfig             = config.HTTPConfig
	CustomerRoutesConfig   = config.CustomerRoutesConfig
	CustomerHTTPScope      = config.CustomerHTTPScope
	ControlPlaneConfig     = config.ControlPlaneConfig
	AuthConfig             = config.AuthConfig
	AuthRateLimit          = config.AuthRateLimit
	MerchantCreationConfig = config.MerchantCreationConfig

	MerchantDeclaration        = config.MerchantDeclaration
	PSPConfig                  = config.PSPConfig
	ProviderRailAccountConfig  = config.ProviderRailAccountConfig
	PSPSignerConfig            = config.PSPSignerConfig
	CustodianConfig            = config.CustodianConfig
	CustodianAccountConfig     = config.CustodianAccountConfig
	MerchantProfileConfig      = config.MerchantProfileConfig
	InvoiceConfig              = config.InvoiceConfig
	CheckoutRoutingRuleConfig  = config.CheckoutRoutingRuleConfig
	CheckoutRoutingMatchConfig = config.CheckoutRoutingMatchConfig
	BillingPolicyConfig        = config.BillingPolicyConfig
	BillingPolicyBindingConfig = config.BillingPolicyBindingConfig
	BudgetWindowConfig         = config.BudgetWindowConfig

	DBConfig              = config.DBConfig
	RedisConfig           = config.RedisConfig
	LoggerConfig          = config.LoggerConfig
	SendGridConfig        = config.SendGridConfig
	RateLimitsConfig      = config.RateLimitsConfig
	RateLimit             = config.RateLimit
	CaptchaConfig         = config.CaptchaConfig
	EncryptionConfig      = config.EncryptionConfig
	VaultConfig           = config.VaultConfig
	AdminConsoleConfig    = config.AdminConsoleConfig
	LLMConfig             = config.LLMConfig
	ProviderSandboxConfig = config.ProviderSandboxConfig
	HyperSwitchConfig     = config.HyperSwitchConfig

	ProviderCredentialSnapshot = config.ProviderCredentialSnapshot
	Cache                      = cache.Cache
	EmailSender                = config.EmailSender
	SMSSender                  = config.SMSSender

	// Identity is who Deps.Authenticate says is calling.
	Identity        = billingauth.Identity
	PrincipalKind   = billingauth.PrincipalKind
	CredentialClass = billingauth.CredentialClass
	// Requirement is the permission, scope and target Deps.Authorize checks.
	Requirement = billingauth.Requirement
	Target      = billingauth.Target
	Scope       = billingauth.Scope
	// DelegatedPrincipal is a customer route's own authentication result: an
	// explicit merchant and paying subject.
	DelegatedPrincipal = billingauth.DelegatedPrincipal
	// GateError lets an authentication hook answer with a specific HTTP status.
	GateError = billingauth.GateError
)

const (
	// Sandbox and Live select which PSP credentials are accepted.
	Sandbox = config.CredentialPostureSandbox
	Live    = config.CredentialPostureLive

	// ProviderWritesFull is normal operation; ProviderWritesLimited makes no
	// system-initiated provider writes; ProviderWritesReadOnly makes none
	// (it never charges anyone).
	ProviderWritesFull     = config.ProviderWriteModeFull
	ProviderWritesLimited  = config.ProviderWriteModeLimited
	ProviderWritesReadOnly = config.ProviderWriteModeReadOnly

	SecretBackendSnapshot = config.SecretBackendSnapshot
	SecretBackendVault    = config.SecretBackendVault
	SecretBackendDB       = config.SecretBackendDB

	RiverManaged   = config.RiverManaged
	RiverHostOwned = config.RiverHostOwned

	CustomerSelfService            = config.CustomerSelfService
	CustomerSubscriptionManagement = config.CustomerSubscriptionManagement
	CustomerBillingManagement      = config.CustomerBillingManagement

	User      = billingauth.User
	Machine   = billingauth.Machine
	Delegated = billingauth.Delegated

	CredentialUserSession = billingauth.CredentialClassUserSession
	CredentialAutomation  = billingauth.CredentialClassAutomation

	MerchantScope = billingauth.MerchantScope
	CustomerScope = billingauth.CustomerScope
	PlatformScope = billingauth.PlatformScope
)

var (
	// ErrUnauthenticated is what Deps.Authenticate returns for a request
	// without a valid credential (401).
	ErrUnauthenticated = billingauth.ErrUnauthenticated
	// ErrForbidden is what Deps.Authorize returns to refuse a permission (403).
	ErrForbidden = billingauth.ErrForbidden
)

// PSPFromEnv declares one PSP from conventionally named variables, so enabling
// a provider is configuration only. With P = upper(key)+"_": P+"RAIL"
// (default: key), P+"ACCOUNT_ID", then P+upper(name) for each of the rail's
// credential slots and settings. Required slots must be set.
func PSPFromEnv(key string, lookup func(string) (string, bool)) (PSPConfig, error) {
	return config.PSPFromEnv(key, lookup)
}
