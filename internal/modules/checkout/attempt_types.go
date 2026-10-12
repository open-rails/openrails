package checkout

import (
	"errors"
)

var (
	ErrCheckoutAttemptValidation       = errors.New("checkout attempt validation failed")
	ErrCheckoutAttemptNotFound         = errors.New("checkout attempt not found")
	ErrCheckoutAttemptForbidden        = errors.New("checkout attempt access denied")
	ErrCheckoutAttemptExpired          = errors.New("checkout attempt expired")
	ErrCheckoutAttemptPending          = errors.New("checkout attempt request already pending")
	ErrCheckoutAttemptConflict         = errors.New("checkout attempt conflict")
	ErrCheckoutAttemptNotSolana        = errors.New("checkout attempt is not a solana session")
	ErrCheckoutAttemptAlreadyCompleted = errors.New("checkout attempt already completed")
)
