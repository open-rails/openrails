package payment

import (
	"errors"
)

var (
	ErrOrderAttemptValidation       = errors.New("order attempt validation failed")
	ErrOrderAttemptNotFound         = errors.New("order attempt not found")
	ErrOrderAttemptForbidden        = errors.New("order attempt access denied")
	ErrOrderAttemptExpired          = errors.New("order attempt expired")
	ErrOrderAttemptPending          = errors.New("order attempt request already pending")
	ErrOrderAttemptConflict         = errors.New("order attempt conflict")
	ErrOrderAttemptNotSolana        = errors.New("order attempt is not a solana session")
	ErrOrderAttemptAlreadyCompleted = errors.New("order attempt already completed")
)
