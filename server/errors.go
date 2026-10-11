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
)
