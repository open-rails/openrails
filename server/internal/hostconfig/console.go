package hostconfig

// ConsolePath is the standalone server's browser console, separate from /v1.
const ConsolePath = "/admin"

// ConsoleConfig declares the apps staff sign in through. The console is
// always served at ConsolePath; local accounts follow LocalSignIn.
type ConsoleConfig struct {
	// Issuers are keyed by a merchant id or name, or "root" for operators.
	Issuers map[string]ConsoleIssuer `koanf:"issuers"`
}
