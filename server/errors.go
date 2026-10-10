package server

import (
	"github.com/open-rails/openrails/internal/merchants"
)

// The errors the server's methods answer, for errors.Is.
var (
	// ErrMerchantNotFound: no merchant has the id. It is also
	// billing.ErrNotFound.
	ErrMerchantNotFound = merchants.ErrMerchantNotFound
	// ErrInvalidMerchantName: the name breaks the merchant name rule.
	ErrInvalidMerchantName = merchants.ErrInvalidName
	// ErrRenamesDisabled: Config.Naming forbids renames.
	ErrRenamesDisabled = merchants.ErrRenamesDisabled

	// ErrInvalidAPIHost: not a bare lowercase domain name.
	ErrInvalidAPIHost = merchants.ErrInvalidAPIHost
	// ErrAPIHostReserved: the host serves the deployment itself.
	ErrAPIHostReserved = merchants.ErrAPIHostReserved
	// ErrAPIHostTaken: another merchant routes from the host.
	ErrAPIHostTaken = merchants.ErrAPIHostTaken
	// ErrAPIHostClaimMissing: the merchant has no open claim to verify.
	ErrAPIHostClaimMissing = merchants.ErrAPIHostClaimMissing
	// ErrAPIHostUnproven: the challenge record does not carry the claim's
	// token yet.
	ErrAPIHostUnproven = merchants.ErrAPIHostUnproven
)
