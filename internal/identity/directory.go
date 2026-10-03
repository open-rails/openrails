// Package identity names the provider-neutral directory contracts used by
// billing. Hosts explicitly supply their identity integration.
package identity

import (
	"github.com/open-rails/openrails/billing"
)

type UserDirectory = billing.UserDirectory
type UsernameResolver = billing.UsernameResolver
