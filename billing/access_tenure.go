package billing

import "time"

// AccessTenureParams asks how long a customer held each of 1 to
// MaxEntitlementChecks keys. At is a past instant; zero uses the service clock.
type AccessTenureParams struct {
	CustomerID   CustomerID
	Entitlements []string
	At           time.Time
}

// AccessTenure is elapsed access as of AsOf, in whole seconds per requested
// entitlement. Each value unions product-access windows with the key's own
// valid-time history. Overlaps count once, future time never counts, and
// revocation, expiration and key retirement end their respective windows.
type AccessTenure struct {
	AsOf    time.Time        `json:"as_of"`
	Seconds map[string]int64 `json:"seconds"`
}
