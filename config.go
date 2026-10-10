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
	// Config is the engine's configuration: plain data. TestMode and
	// ProviderWriteMode are required.
	Config = config.Config
	// Deps is everything the engine reaches outside its process: connections,
	// credentials and the host's hooks.
	Deps = config.Deps

	// CredentialPosture is Config.TestMode: Sandbox or Live.
	CredentialPosture = config.CredentialPosture
	// CheckoutConfig is Config.Checkout: the shared payment page.
	CheckoutConfig = config.CheckoutConfig
	// Routes is the HTTP surface the adapters mount on the root router.
	Routes = config.Routes
	// RouteGroups is Routes.RouteGroups: the route groups turned on.
	RouteGroups = config.RouteGroups
	// Permissions is Routes.Permissions: what a caller must hold for each
	// staff route group.
	Permissions = config.Permissions

	// MerchantDeclaration is Config.Merchant: the one merchant an embedded
	// engine serves.
	MerchantDeclaration = config.MerchantDeclaration
	// MerchantSecrets is MerchantDeclaration.Secrets: the merchant's own
	// credentials (secrets.scim_token).
	MerchantSecrets = config.MerchantSecrets
	// PSPConfig is one PSP of MerchantDeclaration.PSPs: the merchant's account
	// on a rail, with its credentials and settings.
	PSPConfig = config.PSPConfig
	// PSPSignerConfig selects how a Solana PSP signs.
	PSPSignerConfig = config.PSPSignerConfig
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
	// EncryptionConfig is Config.Encryption: the master key for credentials
	// stored in the database.
	EncryptionConfig = config.EncryptionConfig
	// VaultConfig is Config.Vault: the HashiCorp Vault connection OpenRails
	// opens when Deps.Vault is nil.
	VaultConfig = config.VaultConfig
	// LLMConfig is Config.LLM: the model behind the console's natural-language
	// features.
	LLMConfig = config.LLMConfig
	// ProviderSandboxConfig is Config.ProviderSandbox: loopback provider
	// gateways for sandbox runs.
	ProviderSandboxConfig = config.ProviderSandboxConfig
	// HyperSwitchConfig is Config.HyperSwitch: the host's HyperSwitch
	// deployment.
	HyperSwitchConfig = config.HyperSwitchConfig

	// EmailSender is Deps.Email: it delivers OpenRails' rendered email.
	EmailSender = config.EmailSender
	// Email is one message an EmailSender delivers.
	Email = config.Email
	// EmailAddress is a mailbox and its display name.
	EmailAddress = config.EmailAddress

	// UserInfo is Deps.UserInfo: helpers/userinfo's Lookup, the host's
	// directory read in process (AuthKit's Client.UserInfo()). Email notices,
	// the admin customer read, lists and search ask it.
	UserInfo = config.UserInfo

	// Authenticator is Routes.Auth: helpers/auth's Authenticator, which says
	// who a request is (AuthKit's ak.Authenticator()). OpenRails builds its
	// gates from the Verified it returns.
	Authenticator = billingauth.Authenticator
	// Scope is Routes.Scope: where callers hold Permissions (helpers/auth).
	Scope = billingauth.Scope
	// Identity is who the Authenticator says a request is (helpers/auth):
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
)

const (
	// Sandbox accepts only sandbox PSP credentials.
	Sandbox = config.CredentialPostureSandbox
	// Live accepts only live PSP credentials.
	Live = config.CredentialPostureLive

	// ProviderWritesFull is normal operation.
	ProviderWritesFull = config.ProviderWriteModeFull
	// ProviderWritesLimited makes no system-initiated provider writes:
	// dunning, invoice collection and the other proactive operations wait,
	// while checkout and customer or staff actions run.
	ProviderWritesLimited = config.ProviderWriteModeLimited
	// ProviderWritesReadOnly makes no provider writes: it never charges anyone.
	ProviderWritesReadOnly = config.ProviderWriteModeReadOnly

	// SecretBackendSnapshot keeps host-supplied credentials in memory.
	SecretBackendSnapshot = config.SecretBackendSnapshot
	// SecretBackendVault stores credentials in HashiCorp Vault.
	SecretBackendVault = config.SecretBackendVault
	// SecretBackendDB stores credentials encrypted in the database.
	SecretBackendDB = config.SecretBackendDB

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
)

// ErrUnauthenticated refuses a request without a valid credential: the
// helpers/auth sentinel.
var ErrUnauthenticated = billingauth.ErrUnauthenticated

// ParseMerchantDeclaration reads Config.Merchant from one YAML document that
// names its merchant with a required slug, refusing unknown fields.
func ParseMerchantDeclaration(raw []byte) (MerchantDeclaration, error) {
	return config.ParseMerchantDeclaration(raw)
}

// ReadMerchantFile reads Config.Merchant from a YAML file in
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
