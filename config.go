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
	// Routes selects the HTTP surface Client.Routes returns and the adapters
	// mount on the root router.
	Routes = config.Routes
	// AdminConsole is Routes.AdminConsole: the merchant admin console.
	AdminConsole = config.AdminConsole
	// ConsoleIssuer is AdminConsole.Issuer: the trusted issuer staff sign in at.
	ConsoleIssuer = config.ConsoleIssuer
	// CustomerRoutes is one further customer surface of
	// Routes.CustomerProfiles.
	CustomerRoutes = config.CustomerRoutes
	// CustomerHTTPScope is Routes.Customers and CustomerRoutes.Scope: which
	// customer routes a surface mounts.
	CustomerHTTPScope = config.CustomerHTTPScope
	// ControlPlaneConfig is Config.ControlPlane: the OpenRails-owned AuthKit
	// control plane.
	ControlPlaneConfig = config.ControlPlaneConfig
	// AuthConfig is ControlPlaneConfig.Auth: issuer, signing keys and naming.
	AuthConfig = config.AuthConfig
	// AuthRateLimit is one AuthKit rate-limit bucket of
	// ControlPlaneConfig.AuthRateLimits.
	AuthRateLimit = config.AuthRateLimit
	// MerchantCreationConfig is ControlPlaneConfig.MerchantCreation: the
	// policy for merchant names users claim.
	MerchantCreationConfig = config.MerchantCreationConfig
	// ResourceServerConfig is ControlPlaneConfig.ResourceServer: the
	// authorization servers whose access tokens the merchant API accepts.
	ResourceServerConfig = config.ResourceServerConfig
	// TrustedIssuerConfig is one of ResourceServerConfig.TrustedIssuers.
	TrustedIssuerConfig = config.TrustedIssuerConfig
	// NamingConfig is AuthConfig.Naming: the rename policy for merchant names
	// and usernames.
	NamingConfig = config.NamingConfig
	// FormerNamesConfig is NamingConfig.FormerNames: how long a former name
	// keeps forwarding.
	FormerNamesConfig = config.FormerNamesConfig
	// FormerNamesMode is FormerNamesConfig.Mode.
	FormerNamesMode = config.FormerNamesMode

	// MerchantDeclaration is Config.Merchant: the one merchant an embedded
	// engine serves.
	MerchantDeclaration = config.MerchantDeclaration
	// PSPConfig is one PSP of MerchantDeclaration.PSPs: the merchant's account
	// on a rail, with its credentials and settings.
	PSPConfig = config.PSPConfig
	// PSPSignerConfig selects how a Solana PSP signs.
	PSPSignerConfig = config.PSPSignerConfig
	// CustodianConfig is one custodian of MerchantDeclaration.Custodians: the
	// merchant's account with a card custodian.
	CustodianConfig = config.CustodianConfig

	// DBConfig is Config.DB: the Postgres connection OpenRails opens when
	// Deps.Postgres is nil.
	DBConfig = config.DBConfig
	// RedisConfig is Config.Redis: the Redis connection OpenRails opens when
	// Deps.Redis is nil.
	RedisConfig = config.RedisConfig
	// LoggerConfig is Config.Logger: the log level.
	LoggerConfig = config.LoggerConfig
	// SendGridConfig is Config.SendGrid: the built-in SendGrid sender.
	SendGridConfig = config.SendGridConfig
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

	// EmailSender is Deps.Email: it delivers OpenRails' rendered email,
	// billing and control plane alike.
	EmailSender = config.EmailSender
	// Email is one message an EmailSender delivers.
	Email = config.Email
	// EmailAddress is a mailbox and its display name.
	EmailAddress = config.EmailAddress
	// SMSSender is Deps.SMS: it delivers the control plane's AuthKit
	// text messages.
	SMSSender = config.SMSSender

	// Auth is Routes.Auth: the host's auth as net/http middleware in
	// AuthKit's shape (Required, RequirePermission, Sensitive and Identity).
	// OpenRails stacks it on its own routes by tier.
	Auth = billingauth.Auth
	// Identity is who Auth admitted: the Subject whose authority and money is
	// used, the Invoker acting for it when that is someone else, and the
	// Credential it was proven with.
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

	// FormerNamesFinite forwards a former name for FormerNamesConfig.Duration.
	FormerNamesFinite = config.FormerNamesFinite
	// FormerNamesForever forwards a former name indefinitely.
	FormerNamesForever = config.FormerNamesForever
	// FormerNamesImmediate releases a former name at once.
	FormerNamesImmediate = config.FormerNamesImmediate

	// CustomersNone mounts no customer routes.
	CustomersNone = config.CustomersNone
	// CustomerSelfService is the full customer self-service API.
	CustomerSelfService = config.CustomerSelfService
	// CustomerSubscriptionManagement is cancellation, resumption,
	// subscription payment-method changes and invoice collection-method
	// selection only.
	CustomerSubscriptionManagement = config.CustomerSubscriptionManagement
	// CustomerBillingManagement adds billing history, purchased access, saved
	// methods and payment recovery, without checkout or plan purchases.
	CustomerBillingManagement = config.CustomerBillingManagement

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
