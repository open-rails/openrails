package openrails

import (
	"fmt"
	"os"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
)

// The Config and Deps family. These are the only aliases in this package: each
// type is defined once, in internal/config (the auth contract in
// internal/billingauth), and named here for hosts. The field docs are on those
// definitions; gopls and pkg.go.dev show them through the alias.

type (
	// Config is the engine's configuration: plain data. ProviderWriteMode is
	// required.
	Config = config.Config
	// Deps is everything the engine reaches outside its process: connections,
	// credentials and the host's hooks.
	Deps = config.Deps

	// Routes is the HTTP surface the adapters mount on the root router.
	Routes = config.Routes
	// RouteGroups is Routes.RouteGroups: the route groups turned on.
	RouteGroups = config.RouteGroups
	// MerchantDeclaration is one entry of Config.Merchants: a merchant the
	// engine provisions on start, its catalog, and the issuer whose
	// credentials are bound to it.
	MerchantDeclaration = config.MerchantDeclaration
	// MerchantSecrets is MerchantDeclaration.Secrets: the merchant's own
	// credentials (secrets.scim_token).
	MerchantSecrets = config.MerchantSecrets
	// AlertWebhookConfig is one of MerchantDeclaration.AlertWebhooks: where
	// the merchant's operational alerts are posted.
	AlertWebhookConfig = config.AlertWebhookConfig
	// PSPConfig is one PSP of MerchantDeclaration.PSPs: the merchant's account
	// on a rail, with its credentials and settings.
	PSPConfig = config.PSPConfig
	// PSPs is MerchantDeclaration.PSPs: the merchant's PSPs by key.
	PSPs = config.PSPs
	// Custodians is MerchantDeclaration.Custodians: its custodians by key.
	Custodians = config.Custodians
	// CustodianConfig is one custodian of MerchantDeclaration.Custodians: the
	// merchant's account with a card custodian.
	CustodianConfig = config.CustodianConfig

	// DatabaseConfig is Config.Database: the Postgres schemas New creates or
	// upgrades.
	DatabaseConfig = config.DatabaseConfig
	// DBConfig is Config.DB: the Postgres connection OpenRails opens when
	// Deps.Postgres is nil.
	DBConfig = config.DBConfig
	// RedisConfig is Config.Redis: the Redis connection OpenRails opens when
	// Deps.Redis is nil.
	RedisConfig = config.RedisConfig
	// LoggerConfig is Config.Logger: the log level.
	LoggerConfig = config.LoggerConfig
	// SMTPConfig is Config.SMTP: the built-in SMTP sender.
	SMTPConfig = config.SMTPConfig
	// RateLimitsConfig is Config.RateLimits: a limit per bucket name.
	RateLimitsConfig = config.RateLimitsConfig
	// RateLimit is one bucket's requests per minute.
	RateLimit = config.RateLimit
	// CaptchaConfig is Config.Captcha: the captcha account.
	CaptchaConfig = config.CaptchaConfig
	// VaultConfig is Config.Vault: the HashiCorp Vault connection OpenRails
	// opens when Deps.Vault is nil.
	VaultConfig = config.VaultConfig
	// SolanaConfig is Config.Solana: the deployment's Solana crankers.
	SolanaConfig = config.SolanaConfig
	// SolanaCrankerConfig is one of SolanaConfig.Crankers.
	SolanaCrankerConfig = config.SolanaCrankerConfig
	// LLMConfig is Config.LLM: the model behind the console's natural-language
	// features.
	LLMConfig = config.LLMConfig
	// ProviderSandboxConfig is Config.ProviderSandbox: loopback provider
	// gateways for sandbox runs.
	ProviderSandboxConfig = config.ProviderSandboxConfig
	// HyperSwitchConfig is Config.HyperSwitch: the host's HyperSwitch
	// deployment.
	HyperSwitchConfig = config.HyperSwitchConfig
	// PlatformVaultConfig is Config.PlatformVault: the operator's card vault.
	PlatformVaultConfig = config.PlatformVaultConfig

	// EmailSender is Deps.Email: it delivers OpenRails' rendered email.
	EmailSender = config.EmailSender
	// Email is one message an EmailSender delivers.
	Email = config.Email
	// EmailAddress is a mailbox and its display name.
	EmailAddress = config.EmailAddress

	// UserInfo is Deps.UserInfo: authkit/userinfo's Lookup, the host's
	// directory read in process (AuthKit's Client.UserInfo()). Email notices,
	// the admin customer read, lists and search ask it.
	UserInfo = config.UserInfo

	// Authenticator is Routes.Auth: authkit/auth's Authenticator, which says
	// who a request is (AuthKit's ak.Authenticator()). OpenRails builds its
	// gates from the Verified it returns.
	Authenticator = billingauth.Authenticator
	// Identity is who the Authenticator says a request is (authkit/auth):
	// the Subject whose authority and money is used, the Invoker acting for
	// it when that is someone else, and the Credential it was proven with.
	Identity = billingauth.Identity
	// SubjectKind is Identity.SubjectKind: SubjectUser or SubjectApplication.
	SubjectKind = billingauth.SubjectKind
	// Invoker is Identity.Invoker: the party acting for the subject.
	Invoker = billingauth.Invoker
	// Credential is Identity.Credential: how Subject proved itself.
	Credential = billingauth.Credential
	// CredentialKind is Credential.Kind.
	CredentialKind = billingauth.CredentialKind

	// Read names a family of Client reads served from memory.
	Read = config.Read
	// ReadPolicy is how long a Read is served from memory.
	ReadPolicy = config.ReadPolicy
	// ReadCache is Config.ReadCache and RemoteOptions.ReadCache: the policy of
	// each Read it names, replacing its default.
	ReadCache = config.ReadCache
)

// A PSP's environment is its rail's own; omitted, it is the rail's
// real-money one.
const (
	// EnvironmentLive moves real money on nmi, stripe and ccbill.
	EnvironmentLive = config.EnvironmentLive
	// EnvironmentTest is the provider's own system moving no money: an NMI
	// account in test mode, Stripe test keys.
	EnvironmentTest = config.EnvironmentTest
	// EnvironmentSandbox is the provider's separate sandbox system:
	// sandbox.nmi.com, CCBill's sandbox.
	EnvironmentSandbox = config.EnvironmentSandbox
	// EnvironmentMainnet is Solana's real-money network.
	EnvironmentMainnet = config.EnvironmentMainnet
	// EnvironmentDevnet is Solana's devnet.
	EnvironmentDevnet = config.EnvironmentDevnet
	// EnvironmentTestnet is Solana's testnet.
	EnvironmentTestnet = config.EnvironmentTestnet

	// ProviderWritesFull is normal operation.
	ProviderWritesFull = config.ProviderWriteModeFull
	// ProviderWritesLimited makes no system-initiated provider writes:
	// dunning, invoice collection and the other proactive operations wait,
	// while purchase and customer or staff actions run.
	ProviderWritesLimited = config.ProviderWriteModeLimited
	// ProviderWritesReadOnly makes no provider writes: it never charges anyone.
	ProviderWritesReadOnly = config.ProviderWriteModeReadOnly

	// SubjectUser is a user: a customer, or staff.
	SubjectUser = billingauth.SubjectUser
	// SubjectApplication is an application account.
	SubjectApplication = billingauth.SubjectApplication

	// CredentialSession is a signed-in session's token.
	CredentialSession = billingauth.CredentialSession
	// CredentialDeviceKey is a user's enrolled device key.
	CredentialDeviceKey = billingauth.CredentialDeviceKey
	// CredentialAPIKey is an API key of the subject's.
	CredentialAPIKey = billingauth.CredentialAPIKey
	// CredentialSignedToken is a token an application signed with its own
	// key.
	CredentialSignedToken = billingauth.CredentialSignedToken
	// CredentialAccessToken is an authorization server's access token.
	CredentialAccessToken = billingauth.CredentialAccessToken

	// ReadCatalog is GetProduct, ListProducts, GetPrice (without Verify) and
	// ListPrices.
	ReadCatalog = config.ReadCatalog
	// ReadMeters is GetMeter and ListMeters.
	ReadMeters = config.ReadMeters
	// ReadPublicConfig is GetPublicConfig.
	ReadPublicConfig = config.ReadPublicConfig
	// ReadMerchantConfiguration is GetMerchantConfiguration.
	ReadMerchantConfiguration = config.ReadMerchantConfiguration
)

// ErrUnauthenticated refuses a request without a valid credential: the
// authkit/auth sentinel.
var ErrUnauthenticated = billingauth.ErrUnauthenticated

// ParseMerchantDeclaration reads an entry of Config.Merchants from one YAML
// document that names its merchant with a required slug, the entry's key,
// refusing unknown fields. Issuer is set in code.
func ParseMerchantDeclaration(raw []byte) (MerchantDeclaration, error) {
	return config.ParseMerchantDeclaration(raw)
}

// ReadMerchantFile reads an entry of Config.Merchants from a YAML file in
// ParseMerchantDeclaration's shape. The file carries PSP credentials, so keep
// it out of version control.
func ReadMerchantFile(path string) (MerchantDeclaration, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return MerchantDeclaration{}, err
	}
	m, err := config.ParseMerchantDeclaration(raw)
	if err != nil {
		return MerchantDeclaration{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}
