package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	log "github.com/sirupsen/logrus"
)

// FlexiblePort is a custom type that can unmarshal both strings and integers
type FlexiblePort int16

// UnmarshalText implements the encoding.TextUnmarshaler interface
func (p *FlexiblePort) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*p = 0
		return nil
	}

	val, err := strconv.ParseInt(s, 10, 16)
	if err != nil {
		return fmt.Errorf("invalid port value: %w", err)
	}

	*p = FlexiblePort(val)
	return nil
}

const EnvProd string = "prod"
const EnvDev string = "dev"

const ConfigContextKey string = "config"

type Config struct {
	Env         string            `koanf:"env,omitempty"`
	Port        FlexiblePort      `koanf:"port,omitempty"`
	PrivatePort FlexiblePort      `koanf:"private_port,omitempty"` // Private/service API port (default 8060)
	Host        string            `koanf:"host,omitempty"`
	APIKey      string            `koanf:"api_key,omitempty"` // Shared secret for service-to-service auth (X-API-KEY header)
	NMI         *NMIConfig        `koanf:"nmi,omitempty"`
	CCBill      *CCBillConfig     `koanf:"ccbill,omitempty"`
	Webhooks    *WebhookConfig    `koanf:"webhooks,omitempty"`
	Solana      *SolanaConfig     `koanf:"solana,omitempty"`
	Stripe      *StripeConfig     `koanf:"stripe,omitempty"`
	DB          *DBConfig         `koanf:"db,omitempty"`
	Redis       *RedisConfig      `koanf:"redis,omitempty"`
	Auth        *AuthConfig       `koanf:"auth,omitempty"`
	ClickHouse  *ClickHouseConfig `koanf:"clickhouse,omitempty"`
	Logger      *LoggerConfig     `koanf:"logger,omitempty"`
	SendGrid    *SendGridConfig   `koanf:"sendgrid,omitempty"`
	CorsOrigins []string          `koanf:"cors_origins,omitempty"`
	RateLimits  *RateLimitsConfig `koanf:"rate_limits,omitempty"`
}

// DBConfig holds database configuration.
// Supports both legacy connection string (URL) and atomic parameters.
// If URL is provided, it takes precedence. Otherwise, connection string
// is built from individual parameters (Host, Port, Username, etc.).
// Database is always PostgreSQL.
type DBConfig struct {
	// Legacy: Full connection string (optional)
	URL string `koanf:"url"`

	// Atomic parameters (preferred for template-based configuration)
	Host     string `koanf:"host"`
	Port     string `koanf:"port"`
	Database string `koanf:"database"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	SSLMode  string `koanf:"sslmode"`
}

// GetConnectionString returns the database connection string.
// Priority order:
// 1. If URL is set, use it directly
// 2. If all atomic parameters are present, build connection string from them
// 3. Return empty string (caller should use defaults or error based on environment)
func (c *DBConfig) GetConnectionString() string {
	// 1. If legacy URL is provided, use it
	if c.URL != "" {
		return c.URL
	}

	// 2. Build connection string from atomic parameters if all required fields are present
	if c.Host != "" && c.Port != "" && c.Database != "" && c.Username != "" {
		// Format: postgresql://username:password@host:port/database?sslmode=...
		connStr := fmt.Sprintf(
			"postgresql://%s:%s@%s:%s/%s",
			c.Username,
			c.Password,
			c.Host,
			c.Port,
			c.Database,
		)

		// Add query parameters
		params := []string{}

		// Default to sslmode=disable if not specified
		sslMode := c.SSLMode
		if sslMode == "" {
			sslMode = "disable"
		}
		params = append(params, fmt.Sprintf("sslmode=%s", sslMode))

		if len(params) > 0 {
			connStr += "?" + strings.Join(params, "&")
		}

		return connStr
	}

	// 3. No URL and incomplete atomic parameters - return empty (caller handles defaults)
	return ""
}

type NMIConfig struct {
	SecurityKey     string                        `koanf:"security_key"`
	TokenizationKey string                        `koanf:"tokenization_key"`
	WebhookSecret   string                        `koanf:"webhook_secret"`
	TestMode        bool                          `koanf:"test_mode"`
	DirectPostURL   string                        `koanf:"direct_post_url"`
	QueryURL        string                        `koanf:"query_url"`
	Providers       map[string]*NMIProviderConfig `koanf:"providers"`
}

type NMIProviderConfig struct {
	SecurityKey     string `koanf:"security_key"`
	TokenizationKey string `koanf:"tokenization_key"`
	WebhookSecret   string `koanf:"webhook_secret"`
	TestMode        *bool  `koanf:"test_mode"`
	DirectPostURL   string `koanf:"direct_post_url"`
	QueryURL        string `koanf:"query_url"`
}

type StripeConfig struct {
	SecretKey     string `koanf:"secret_key"`
	WebhookSecret string `koanf:"webhook_secret"`
	SuccessURL    string `koanf:"success_url"`
	CancelURL     string `koanf:"cancel_url"`
}

type NMIProviderSettings struct {
	Name            string
	SecurityKey     string
	TokenizationKey string
	WebhookSecret   string
	TestMode        bool
	DirectPostURL   string
	QueryURL        string
}

func (cfg *NMIConfig) ProviderSettings(name string) (*NMIProviderSettings, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nmi configuration is missing")
	}

	providerKey := strings.TrimSpace(strings.ToLower(name))
	if providerKey == "" {
		providerKey = "mobius"
	}

	provider, ok := cfg.Providers[providerKey]
	if !ok || provider == nil {
		return nil, fmt.Errorf("nmi provider '%s' is not configured", providerKey)
	}

	settings := &NMIProviderSettings{
		Name:            providerKey,
		SecurityKey:     firstNonEmpty(provider.SecurityKey, cfg.SecurityKey),
		TokenizationKey: firstNonEmpty(provider.TokenizationKey, cfg.TokenizationKey),
		WebhookSecret:   firstNonEmpty(provider.WebhookSecret, cfg.WebhookSecret),
		DirectPostURL:   firstNonEmpty(provider.DirectPostURL, cfg.DirectPostURL),
		QueryURL:        firstNonEmpty(provider.QueryURL, cfg.QueryURL),
		TestMode:        cfg.TestMode,
	}
	if provider.TestMode != nil {
		settings.TestMode = *provider.TestMode
	}

	if settings.SecurityKey == "" {
		return nil, fmt.Errorf("nmi provider '%s' security key is required", providerKey)
	}
	if settings.WebhookSecret == "" {
		log.Warnf("nmi provider '%s' webhook secret is not configured; signature validation will be disabled", providerKey)
	}

	return settings, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

type CCBillConfig struct {
	Salt               string `koanf:"salt"`
	ClientSubAcc       string `koanf:"client_sub_acc"`
	ClientAccNum       string `koanf:"client_acc_num"`
	SubscriptionTypeId string `koanf:"subscription_type_id"`
	TestMode           bool   `koanf:"test_mode"`

	DataLinkUsername string `koanf:"datalink_username"`
	DataLinkPassword string `koanf:"datalink_password"`
}

type RedisConfig struct {
	Addr     string `koanf:"addr"`
	Password string `koanf:"password"`
	DB       int    `koanf:"db"`
}

// AuthConfig holds JWT verification configuration for billing service.
// Billing is a JWT verifier (not issuer) - it validates tokens issued by doujins/hentai0.
type AuthConfig struct {
	Issuers          []string `koanf:"issuers"`           // List of expected token issuers (e.g., ["https://doujins.com", "https://hentai0.com"])
	ExpectedAudience string   `koanf:"expected_audience"` // Accept token only if it contains this audience (e.g., "billing-app")
}

type SolanaConfig struct {
	RPCEndpoint     string `koanf:"rpc_endpoint"`
	Network         string `koanf:"network"` // mainnet, devnet, testnet
	RecipientWallet string `koanf:"recipient_wallet"`

	SupportedTokens map[string]TokenConfig `koanf:"supported_tokens,omitempty"`

	TransactionTimeoutSeconds int     `koanf:"transaction_timeout_seconds,omitempty"`
	ConfirmationBlocks        int     `koanf:"confirmation_blocks,omitempty"`
	MaxTransactionFee         float64 `koanf:"max_transaction_fee,omitempty"`
}

// TokenConfig defines configuration for a specific Solana token
type TokenConfig struct {
	Mint        string  `json:"mint" koanf:"mint"`         // Token mint address
	Symbol      string  `json:"symbol" koanf:"symbol"`     // Token symbol (e.g., "SOL", "USDC")
	Name        string  `json:"name" koanf:"name"`         // Token name
	Decimals    int     `json:"decimals" koanf:"decimals"` // Token decimal places
	Price       float64 `json:"price"`                     // Price in USD (fetched from Jupiter at runtime, not loaded from config)
	Enabled     bool    `json:"enabled" koanf:"enabled"`   // Whether this token is enabled
	MainnetMint string  `json:"mainnet_mint,omitempty" koanf:"mainnet_mint"`
}

// RateLimitsConfig is a map of endpoint identifier -> rate limit config
type RateLimitsConfig map[string]*RateLimit

// SendGridConfig holds SendGrid email configuration
type SendGridConfig struct {
	APIKey    string `koanf:"api_key"`
	FromEmail string `koanf:"from_email"`
	FromName  string `koanf:"from_name"`
}

type ClickHouseConfig struct {
	HTTPAddr   string `koanf:"http_addr"`   // HTTP address for queries, e.g., http://clickhouse:8123
	ClientAddr string `koanf:"client_addr"` // Native client address, e.g., clickhouse:9000
	Database   string `koanf:"db"`          // ClickHouse database name (e.g., analytics)
	Username   string `koanf:"user"`        // Optional username for authentication
	Password   string `koanf:"password"`    // Optional password for authentication
	Cluster    string `koanf:"cluster"`     // ClickHouse cluster name (e.g., doujins)
}

// LoggerConfig holds logging configuration
type LoggerConfig struct {
	Level string `koanf:"level"` // debug | info | error
}

// RateLimit defines a rate limit policy
type RateLimit struct {
	Limit  int           `koanf:"limit"`
	Window time.Duration `koanf:"window"`
}

// WebhookConfig is kept for backwards compatibility but webhook retry is no longer used.
// Webhook processing is now synchronous-only - payment processors retry on their end.
type WebhookConfig struct {
	// Deprecated: Retry config is no longer used. Webhooks are processed synchronously.
	Retry WebhookRetryConfig `koanf:"retry"`
}

// WebhookRetryConfig is deprecated - webhook retry mechanism has been removed.
// Keeping the struct for backwards compatibility with existing config files.
type WebhookRetryConfig struct {
	MaxAttempts    int           `koanf:"max_attempts"`
	InitialBackoff time.Duration `koanf:"initial_backoff"`
	MaxBackoff     time.Duration `koanf:"max_backoff"`
	BatchSize      int           `koanf:"batch_size"`
}

// Validate validates the billing configuration
func Validate(cfg *Config) error {
	// Skip strict validation in development environments
	isDev := cfg.Env == "development" || cfg.Env == "dev" || cfg.Env == ""

	if !isDev {
		if err := validateNMI(cfg.NMI); err != nil {
			return fmt.Errorf("nmi config validation failed: %w", err)
		}

		// Validate CCBill configuration
		if err := validateCCBill(cfg.CCBill); err != nil {
			return fmt.Errorf("ccbill config validation failed: %w", err)
		}
	}

	// Note: test_mode is allowed in production for safe payment processor testing

	// Always validate database configuration
	if err := validateDatabase(cfg.DB); err != nil {
		return fmt.Errorf("database config validation failed: %w", err)
	}

	if err := validateStripe(cfg.Stripe); err != nil {
		return fmt.Errorf("stripe config validation failed: %w", err)
	}

	// Note: Webhook retry config validation removed - webhooks are now synchronous-only

	return nil
}

// validateWebhookConfig is deprecated - webhook retry config is no longer used.
// Keeping as no-op for backwards compatibility.
func validateWebhookConfig(cfg *Config) error {
	return nil
}

func validateStripe(cfg *StripeConfig) error {
	if cfg == nil {
		return nil
	}
	if strings.TrimSpace(cfg.SecretKey) == "" {
		log.Warn("stripe secret key not configured; checkout will be unavailable")
	}
	if strings.TrimSpace(cfg.WebhookSecret) == "" {
		log.Warn("stripe webhook secret not configured; signature verification will be disabled")
	}
	return nil
}

// GetWebhookRetryConfig is deprecated - webhook retry mechanism has been removed.
// Returns empty config. Kept for backwards compatibility.
func (cfg *Config) GetWebhookRetryConfig() WebhookRetryConfig {
	return WebhookRetryConfig{}
}

// validateNMI validates NMI-specific configuration
func validateNMI(cfg *NMIConfig) error {
	if cfg == nil {
		return fmt.Errorf("nmi configuration is required")
	}

	if cfg.DirectPostURL != "" {
		if _, err := url.Parse(cfg.DirectPostURL); err != nil {
			return fmt.Errorf("invalid nmi direct_post_url: %w", err)
		}
	}

	if cfg.QueryURL != "" {
		if _, err := url.Parse(cfg.QueryURL); err != nil {
			return fmt.Errorf("invalid nmi query_url: %w", err)
		}
	}

	if len(cfg.Providers) == 0 {
		return fmt.Errorf("at least one nmi provider must be configured")
	}

	for name, provider := range cfg.Providers {
		if provider == nil {
			return fmt.Errorf("nmi provider '%s' configuration is missing", name)
		}
		if provider.SecurityKey == "" && cfg.SecurityKey == "" {
			return fmt.Errorf("nmi provider '%s' security key is required", name)
		}
		if provider.WebhookSecret == "" && cfg.WebhookSecret == "" {
			return fmt.Errorf("nmi provider '%s' webhook secret is recommended for security", name)
		}
		if provider.DirectPostURL != "" {
			if _, err := url.Parse(provider.DirectPostURL); err != nil {
				return fmt.Errorf("invalid nmi provider '%s' direct_post_url: %w", name, err)
			}
		}
		if provider.QueryURL != "" {
			if _, err := url.Parse(provider.QueryURL); err != nil {
				return fmt.Errorf("invalid nmi provider '%s' query_url: %w", name, err)
			}
		}
	}

	return nil
}

// validateCCBill validates CCBill-specific configuration
func validateCCBill(cfg *CCBillConfig) error {
	if cfg == nil {
		return fmt.Errorf("ccbill configuration is required")
	}

	// Basic required fields
	if cfg.ClientAccNum == "" {
		return fmt.Errorf("ccbill client account number is required")
	}

	if cfg.ClientSubAcc == "" {
		return fmt.Errorf("ccbill client sub account is required")
	}

	if (cfg.DataLinkUsername == "") != (cfg.DataLinkPassword == "") {
		return fmt.Errorf("both datalink username and password must be provided when configuring DataLink access")
	}

	return nil
}

// assembleDBURL builds the database URL from atomic parameters if not explicitly set
func assembleDBURL(cfg *Config) {
	if cfg.DB == nil {
		return
	}

	// If URL is already explicitly set, nothing to do
	if cfg.DB.URL != "" {
		log.Debug("Using explicitly configured DB_URL")
		return
	}

	// Assemble URL from atomic parameters
	// All parameters have defaults from GetDefaultBillingConfig(), so they should all be present
	connStr := fmt.Sprintf(
		"postgresql://%s:%s@%s:%s/%s?sslmode=%s",
		cfg.DB.Username,
		cfg.DB.Password,
		cfg.DB.Host,
		cfg.DB.Port,
		cfg.DB.Database,
		cfg.DB.SSLMode,
	)

	cfg.DB.URL = connStr

	// Log warnings for critical default values being used
	warnings := []string{}
	if cfg.DB.Host == "localhost" {
		warnings = append(warnings, "DB host")
	}
	if cfg.DB.Username == "admin" {
		warnings = append(warnings, "DB username")
	}
	if cfg.DB.Password == "admin_password" {
		warnings = append(warnings, "DB password")
	}
	if cfg.DB.Database == "doujins_db" {
		warnings = append(warnings, "DB database name")
	}

	if len(warnings) > 0 {
		log.Warnf("Using default values for: %s. Assembled DB URL: %s",
			strings.Join(warnings, ", "), connStr)
	} else {
		log.Debugf("Assembled DB URL from configured parameters: %s", connStr)
	}
}

// validateDatabase validates database configuration
func validateDatabase(cfg *DBConfig) error {
	if cfg == nil {
		return fmt.Errorf("database configuration is required")
	}

	// Database is always PostgreSQL
	// After assembleDBURL, cfg.URL should always be set
	if cfg.URL == "" {
		return fmt.Errorf("database URL could not be determined")
	}

	return nil
}

// GetDefaultBillingConfig returns a billing configuration with sensible defaults
func GetDefaultBillingConfig() *Config {
	return &Config{
		Env:         "development",
		Host:        "0.0.0.0",
		Port:        2053,
		PrivatePort: 8060, // Private/service API port (internal only)
		DB: &DBConfig{
			Host:     "localhost",
			Port:     "5432",
			Database: "doujins_db",
			Username: "admin",
			Password: "admin_password",
			SSLMode:  "disable",
		},
		Redis: &RedisConfig{
			// Match docker-compose Garnet (service: garnet)
			Addr:     "garnet:6379",
			Password: "",
			DB:       0,
		},
		Auth: &AuthConfig{
			Issuers:          []string{"http://doujins:2052", "http://doujins:4000"}, // Accept tokens from both doujins and hentai0
			ExpectedAudience: "billing-app",
		},
		ClickHouse: &ClickHouseConfig{
			HTTPAddr:   "http://localhost:8123",
			ClientAddr: "localhost:9000",
			Database:   "analytics",
			Username:   "analytics_user",     // Match docker-compose CLICKHOUSE_USER
			Password:   "analytics_password", // Match docker-compose CLICKHOUSE_PASSWORD
			Cluster:    "doujins",            // Match docker-compose cluster name
		},
		Logger: &LoggerConfig{
			Level: "info", // Default to info level (options: debug, info, warn, error, fatal, panic)
		},
		RateLimits: &RateLimitsConfig{
			"subscribe": &RateLimit{
				Limit:  10, // Very restrictive for payment endpoints
				Window: time.Minute,
			},
			"checkout": &RateLimit{
				Limit:  5, // Heavy rate limiting for checkout - prevents abuse
				Window: time.Minute,
			},
			"webhook": &RateLimit{
				Limit:  100, // Higher for webhooks
				Window: time.Minute,
			},
			"payment": &RateLimit{
				Limit:  20,
				Window: time.Minute,
			},
			"default": &RateLimit{
				Limit:  60,
				Window: time.Minute,
			},
		},
		Solana: &SolanaConfig{},
	}
}

func loadConfigIfExists(k *koanf.Koanf, path string) error {
	if path == "" {
		return nil
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		candidates = append(candidates, filepath.Join("config", path))
		candidates = append(candidates, filepath.Join("./config", path))
	}
	visited := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := visited[candidate]; ok {
			continue
		}
		visited[candidate] = struct{}{}
		if _, err := os.Stat(candidate); err == nil {
			if err := k.Load(file.Provider(candidate), yaml.Parser()); err != nil {
				return fmt.Errorf("loading config file %s: %w", candidate, err)
			}
			return nil
		}
	}
	return nil
}

func Load(configPath string) (*Config, error) {
	k := koanf.New(".")

	// Start from sensible defaults so zero-config works in containers/compose.
	cfg := GetDefaultBillingConfig()

	if err := godotenv.Load(); err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return nil, err
		}
	}

	if configPath == "" {
		if envPath := strings.TrimSpace(os.Getenv("BILLING_CONFIG")); envPath != "" {
			configPath = envPath
		} else {
			configPath = "config.yaml"
		}
	}

	if err := loadConfigIfExists(k, configPath); err != nil {
		return nil, err
	}

	// Load environment variables using koanf's env provider
	// Transform env var names to config keys (e.g., DB_URL -> db.url)
	envCallback := func(s string) string {
		s = strings.ToLower(s)

		// Special case: ENVIRONMENT -> env
		if s == "environment" {
			return "env"
		}

		// Special case: NMI_PROVIDERS_<PROVIDER>_<KEY> -> nmi.providers.<provider>.<key>
		// Example: NMI_PROVIDERS_MOBIUS_SECURITY_KEY -> nmi.providers.mobius.security_key
		if strings.HasPrefix(s, "nmi_providers_") {
			parts := strings.SplitN(s, "_", 4) // ["nmi", "providers", "mobius", "security_key"]
			if len(parts) == 4 {
				return fmt.Sprintf("nmi.providers.%s.%s", parts[2], parts[3])
			}
		}

		// Standard transformation: Replace first underscore with dot
		// DB_URL -> db.url
		// REDIS_ADDR -> redis.addr
		// CLICKHOUSE_HTTP_ADDR -> clickhouse.http_addr
		// CCBILL_SALT -> ccbill.salt
		if !strings.Contains(s, "_") {
			return s // No underscore, return as-is
		}
		return strings.Replace(s, "_", ".", 1)
	}

	if err := k.Load(env.Provider("", ".", envCallback), nil); err != nil {
		return nil, fmt.Errorf("loading environment variables: %w", err)
	}

	// Unmarshal into config struct (overlay onto defaults)
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	if cfg.Solana == nil {
		cfg.Solana = &SolanaConfig{}
	}
	if cfg.Solana.Network == "" {
		cfg.Solana.Network = "mainnet"
	}
	if len(cfg.Solana.SupportedTokens) == 0 {
		cfg.Solana.SupportedTokens = TokensForNetwork(cfg.Solana.Network)
	}

	if cfg.NMI == nil {
		cfg.NMI = &NMIConfig{}
	}
	if cfg.NMI.Providers == nil {
		cfg.NMI.Providers = make(map[string]*NMIProviderConfig)
	}
	if len(cfg.NMI.Providers) > 0 {
		normalized := make(map[string]*NMIProviderConfig, len(cfg.NMI.Providers))
		for name, provider := range cfg.NMI.Providers {
			key := strings.TrimSpace(strings.ToLower(name))
			if key == "" {
				log.Warnf("ignoring NMI provider with empty name (original key: %q)", name)
				continue
			}

			if existing, exists := normalized[key]; exists && existing != nil {
				log.Warnf("duplicate NMI provider configuration detected for key '%s'; overriding previous value", key)
			}

			normalized[key] = provider
		}
		cfg.NMI.Providers = normalized
	}

	// Assemble DB URL from pieces if not explicitly set
	assembleDBURL(cfg)

	// Validate the loaded configuration
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}
