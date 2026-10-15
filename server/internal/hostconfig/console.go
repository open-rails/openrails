package hostconfig

// ConsolePath is the standalone server's browser console, separate from /v1.
const ConsolePath = "/admin"

// ConsoleConfig controls the standalone console at ConsolePath and declares
// the apps staff sign in through. Local accounts follow LocalSignIn separately.
type ConsoleConfig struct {
	// Disabled omits the console's pages, assets and bootstrap endpoints.
	// The default keeps the console available; billing and auth APIs are separate.
	Disabled bool `koanf:"disabled"`
	// Issuers are keyed by a merchant id or name, or "root" for operators.
	Issuers map[string]ConsoleIssuer `koanf:"issuers"`
}
