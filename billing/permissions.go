package billing

// The OAuth scopes of a trusted issuer's access tokens: ScopeMerchant reaches
// the merchant API, ScopeSelf a customer's own billing (/v1/me).
const (
	ScopeMerchant = "openrails:merchant"
	ScopeSelf     = "openrails:self"
)

// Platform-operator (root) permissions: the standalone server's
// cross-merchant directory and root-only operational overrides.
const (
	RootMerchantsRead    = "root:merchants:read"
	RootMerchantsDelete  = "root:merchants:delete"
	RootMerchantsRestore = "root:merchants:restore"
	// RootWorkerHealthRead gates the cross-merchant worker-health view, whose
	// last_error text is another merchant's verbatim job error.
	RootWorkerHealthRead      = "root:worker-health:read"
	RootAdminRateLimitsUnlock = "root:admin-rate-limits:unlock"
)
