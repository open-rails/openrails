package decline

import (
	"github.com/open-rails/openrails/billing"
)

type reasonSpec struct {
	category  Category
	action    Action
	transient bool // the processor asked to try again shortly
}

// reasons is the policy for each public reason; the buyer's copy lives with
// the reason (billing.DeclineReason.Failure). Actions follow #1108 decision 2:
//   - retry: issuer soft/generic declines (Visa category 2/4: retries allowed)
//     and our or the gateway's errors;
//   - fix_payment_method: bad card data, and codes that forbid re-attempts on
//     the same card number (Visa category 1, Mastercard MAC 03/21); a new card
//     from the customer or the account updater resumes dunning;
//   - non_recoverable: stolen, fraud, and a revoked recurring mandate.
//
// 250/251 wait for a new card by owner decision (or#870): losing a wallet must
// not cost a subscription.
var reasons = map[billing.DeclineReason]reasonSpec{
	billing.DeclineGeneric:           {category: IssuerSoft, action: Retry},
	billing.DeclineDoNotHonor:        {category: IssuerSoft, action: Retry},
	billing.DeclineInsufficientFunds: {category: IssuerSoft, action: Retry},
	billing.DeclineOverLimit:         {category: IssuerSoft, action: Retry},
	billing.DeclineCallIssuer:        {category: IssuerSoft, action: Retry},
	billing.DeclineRetryLater:        {category: IssuerSoft, action: Retry},
	billing.DeclineTryAgainLater:     {category: IssuerSoft, action: Retry, transient: true},
	billing.DeclineSecurityViolation: {category: IssuerHard, action: Retry},
	billing.DeclineRestrictedCard:    {category: IssuerHard, action: Retry},

	billing.DeclineIncorrectNumber:      {category: CardData, action: FixPaymentMethod},
	billing.DeclineNoSuchIssuer:         {category: CardData, action: FixPaymentMethod},
	billing.DeclineInvalidAccount:       {category: CardData, action: FixPaymentMethod},
	billing.DeclineExpiredCard:          {category: CardData, action: FixPaymentMethod},
	billing.DeclineInvalidExpiry:        {category: CardData, action: FixPaymentMethod},
	billing.DeclineIncorrectCVC:         {category: CardData, action: FixPaymentMethod},
	billing.DeclineIncorrectZip:         {category: CardData, action: FixPaymentMethod},
	billing.DeclineIncorrectAddress:     {category: CardData, action: FixPaymentMethod},
	billing.DeclineInvalidPIN:           {category: CardData, action: FixPaymentMethod},
	billing.DeclineUpdateCardholderData: {category: CardData, action: FixPaymentMethod},

	billing.DeclineTransactionNotAllowed:  {category: IssuerHard, action: FixPaymentMethod},
	billing.DeclineCardNotSupported:       {category: IssuerHard, action: FixPaymentMethod},
	billing.DeclineCurrencyNotSupported:   {category: IssuerHard, action: FixPaymentMethod},
	billing.DeclineAuthenticationRequired: {category: IssuerHard, action: FixPaymentMethod},
	billing.DeclinePickupCard:             {category: IssuerHard, action: FixPaymentMethod},
	billing.DeclineLostCard:               {category: IssuerHard, action: FixPaymentMethod},
	billing.DeclineStolenCard:             {category: IssuerHard, action: NonRecoverable},
	billing.DeclineFraudulent:             {category: IssuerHard, action: NonRecoverable},
	billing.DeclineStopRecurring:          {category: IssuerHard, action: NonRecoverable},

	billing.DeclineGatewayRejected: {category: GatewayRule, action: Retry},
	billing.DeclineBlockedByPSP:    {category: GatewayRule, action: FixPaymentMethod},

	billing.DeclineProcessingError:      {category: SystemError, action: Retry, transient: true},
	billing.DeclineCommunicationError:   {category: SystemError, action: Retry, transient: true},
	billing.DeclineIssuerUnavailable:    {category: SystemError, action: Retry, transient: true},
	billing.DeclineInvalidRequest:       {category: SystemError, action: Retry},
	billing.DeclineMerchantConfig:       {category: SystemError, action: Retry},
	billing.DeclineDuplicateTransaction: {category: SystemError, action: Retry},

	billing.DeclineUnknown: {category: Unknown, action: Retry},
}

// ProviderFault reports a refusal another card cannot fix: the gateway, the
// processor or our configuration refused it.
func ProviderFault(reason billing.DeclineReason) bool {
	return (reasons[reason].category == SystemError && reason != billing.DeclineDuplicateTransaction) || reason == billing.DeclineGatewayRejected
}
