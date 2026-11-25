package payment

import (
	"errors"
)

var (
	ErrInvoiceAttemptValidation       = errors.New("invoice attempt validation failed")
	ErrInvoiceAttemptNotFound         = errors.New("invoice attempt not found")
	ErrInvoiceAttemptForbidden        = errors.New("invoice attempt access denied")
	ErrInvoiceAttemptExpired          = errors.New("invoice attempt expired")
	ErrInvoiceAttemptPending          = errors.New("invoice attempt request already pending")
	ErrInvoiceAttemptConflict         = errors.New("invoice attempt conflict")
	ErrInvoiceAttemptNotSolana        = errors.New("invoice attempt is not a solana session")
	ErrInvoiceAttemptAlreadyCompleted = errors.New("invoice attempt already completed")
)
