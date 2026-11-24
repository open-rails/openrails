package decline

import (
	"github.com/open-rails/openrails/billing"
)

type reasonSpec struct {
	cause     billing.DeclineCause
	action    Action
	transient bool // the processor asked to try again shortly
}

const (
	customer   = billing.DeclineCauseCustomer
	issuer     = billing.DeclineCauseIssuer
	fraud      = billing.DeclineCauseFraud
	merchant   = billing.DeclineCauseMerchant
	processing = billing.DeclineCauseProcessing
)

// reasons is the policy for each public reason: who caused it and what
// dunning does. The buyer's copy lives with the reason
// (billing.DeclineReason.Failure). Actions:
//   - retry: issuer soft/generic declines (Visa category 2/4) and our or the
//     gateway's errors;
//   - fix_payment_method: bad card data, codes that forbid re-attempts on the
//     same card number, and a retry the networks' advice stops (advice.go); a
//     new card from the customer or the account updater resumes dunning;
//   - non_recoverable: stolen, fraud, and a revoked recurring mandate.
//
// Pick-up and lost cards (250/251) wait for a new card: losing a wallet must
// not cost a subscription.
var reasons = map[billing.DeclineReason]reasonSpec{
	billing.DeclineInsufficientFunds:      {cause: customer, action: Retry},
	billing.DeclineOverLimit:              {cause: customer, action: Retry},
	billing.DeclineIncorrectNumber:        {cause: customer, action: FixPaymentMethod},
	billing.DeclineNoSuchIssuer:           {cause: customer, action: FixPaymentMethod},
	billing.DeclineInvalidAccount:         {cause: customer, action: FixPaymentMethod},
	billing.DeclineExpiredCard:            {cause: customer, action: FixPaymentMethod},
	billing.DeclineInvalidExpiry:          {cause: customer, action: FixPaymentMethod},
	billing.DeclineIncorrectCVC:           {cause: customer, action: FixPaymentMethod},
	billing.DeclineIncorrectZip:           {cause: customer, action: FixPaymentMethod},
	billing.DeclineIncorrectAddress:       {cause: customer, action: FixPaymentMethod},
	billing.DeclineInvalidPIN:             {cause: customer, action: FixPaymentMethod},
	billing.DeclineUpdateCardholderData:   {cause: customer, action: FixPaymentMethod},
	billing.DeclineAuthenticationRequired: {cause: customer, action: FixPaymentMethod},
	billing.DeclineLostCard:               {cause: customer, action: FixPaymentMethod},
	billing.DeclineStopRecurring:          {cause: customer, action: NonRecoverable},

	billing.DeclineGeneric:               {cause: issuer, action: Retry},
	billing.DeclineDoNotHonor:            {cause: issuer, action: Retry},
	billing.DeclineCallIssuer:            {cause: issuer, action: Retry},
	billing.DeclineRetryLater:            {cause: issuer, action: Retry},
	billing.DeclineTryAgainLater:         {cause: issuer, action: Retry, transient: true},
	billing.DeclineRestrictedCard:        {cause: issuer, action: Retry},
	billing.DeclineTransactionNotAllowed: {cause: issuer, action: FixPaymentMethod},
	billing.DeclineCardNotSupported:      {cause: issuer, action: FixPaymentMethod},
	billing.DeclineCurrencyNotSupported:  {cause: issuer, action: FixPaymentMethod},

	billing.DeclineSuspectedFraud:    {cause: fraud, action: Retry},
	billing.DeclineSecurityViolation: {cause: fraud, action: Retry},
	billing.DeclinePickupCard:        {cause: fraud, action: FixPaymentMethod},
	billing.DeclineStolenCard:        {cause: fraud, action: NonRecoverable},
	billing.DeclineFraudulent:        {cause: fraud, action: NonRecoverable},

	billing.DeclineGatewayRejected:      {cause: merchant, action: Retry},
	billing.DeclineBlockedByPSP:         {cause: merchant, action: FixPaymentMethod},
	billing.DeclineBrandNotAccepted:     {cause: merchant, action: FixPaymentMethod},
	billing.DeclineMerchantConfig:       {cause: merchant, action: Retry},
	billing.DeclineInvalidRequest:       {cause: merchant, action: Retry},
	billing.DeclineDuplicateTransaction: {cause: merchant, action: Retry},

	billing.DeclineProcessingError:    {cause: processing, action: Retry, transient: true},
	billing.DeclineCommunicationError: {cause: processing, action: Retry, transient: true},
	billing.DeclineIssuerUnavailable:  {cause: processing, action: Retry, transient: true},

	billing.DeclineUnknown: {cause: billing.DeclineCauseUnknown, action: Retry},
}

// CauseOf is a reason's cause; a reason no table produces is unknown.
func CauseOf(reason billing.DeclineReason) billing.DeclineCause {
	if spec, ok := reasons[reason]; ok {
		return spec.cause
	}
	return billing.DeclineCauseUnknown
}

// HolderMayHoldNewCard reports a refusal that says the card was reissued: its
// holder's updater may already hold the new one.
func HolderMayHoldNewCard(reason billing.DeclineReason) bool {
	return reason == billing.DeclineExpiredCard || reason == billing.DeclineUpdateCardholderData
}

// ProviderFault reports a refusal another card cannot fix: the gateway, the
// processor or our configuration refused it.
func ProviderFault(reason billing.DeclineReason) bool {
	switch reason {
	case billing.DeclineGatewayRejected, billing.DeclineMerchantConfig, billing.DeclineInvalidRequest:
		return true
	}
	return reasons[reason].cause == processing
}
