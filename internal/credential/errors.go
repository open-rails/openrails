package credential

import "errors"

var ErrServiceCredentialHostMismatch = errors.New("controlplane: API key merchant does not match request host")
var ErrServiceCredentialMerchantUnresolved = errors.New("controlplane: API key caller owns no active merchant")
var ErrServiceCredentialScopeDenied = errors.New("controlplane: API key resource scope denied")
var ErrMerchantAmbiguous = errors.New("controlplane: user belongs to multiple merchants")
