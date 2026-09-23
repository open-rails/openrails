package credential

import "errors"

var ErrServiceCredentialHostMismatch = errors.New("controlplane: API key merchant does not match request host")
var ErrServiceCredentialMerchantUnresolved = errors.New("controlplane: API key caller owns no active merchant")
var ErrServiceCredentialScopeDenied = errors.New("controlplane: API key resource scope denied")
var ErrNotRemoteApplicationToken = errors.New("controlplane: not a remote application access token")
var ErrDelegatedNotConfigured = errors.New("controlplane: delegated access verifier not configured")
var ErrDelegatedInvalid = errors.New("controlplane: invalid delegated access token")
var ErrDelegatedUnavailable = errors.New("controlplane: delegated verification unavailable")
var ErrMerchantAmbiguous = errors.New("controlplane: user belongs to multiple merchants")
var ErrDelegatedIssuerUnknown = errors.New("controlplane: delegated token issuer maps to no active merchant")
