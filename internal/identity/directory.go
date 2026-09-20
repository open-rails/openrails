// Package identity names the provider-neutral directory contracts used by
// billing. Hosts explicitly supply their identity integration.
package identity

import "github.com/open-rails/openrails"

type UserDirectory = openrails.UserDirectory
type UsernameResolver = openrails.UsernameResolver
