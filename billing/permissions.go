package billing

// The OAuth scopes of a trusted issuer's access tokens: ScopeMerchant reaches
// the admin API, ScopeSelf a customer's own billing (/v1/me).
const (
	ScopeMerchant = "openrails:merchant"
	ScopeSelf     = "openrails:self"
)

// Platform-operator (root) permissions of the standalone server's accounts.
// OpenRails serves no operator route; a hosted product gates its own operator
// pages with them (the server's HasRootPermission).
const (
	RootMerchantsRead    = "root:merchants:read"
	RootMerchantsDelete  = "root:merchants:delete"
	RootMerchantsRestore = "root:merchants:restore"
	// RootWorkerHealthRead is for a worker-health view, whose last_error
	// text can name any merchant's records.
	RootWorkerHealthRead      = "root:worker-health:read"
	RootAdminRateLimitsUnlock = "root:admin-rate-limits:unlock"
)
