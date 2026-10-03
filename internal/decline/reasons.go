package decline

import "github.com/open-rails/openrails"

type reasonSpec struct {
	category  Category
	action    Action
	transient bool // the processor asked to try again shortly
}

// reasons is the policy for each public reason; the buyer's copy lives with
// the reason (openrails.DeclineReason.Failure). Actions follow #1108 decision 2:
//   - retry: issuer soft/generic declines (Visa category 2/4: retries allowed)
//     and our or the gateway's errors;
//   - fix_payment_method: bad card data, and codes that forbid re-attempts on
//     the same card number (Visa category 1, Mastercard MAC 03/21); a new card
//     from the customer or the account updater resumes dunning;
//   - non_recoverable: stolen, fraud, and a revoked recurring mandate.
//
// 250/251 wait for a new card by owner decision (or#870): losing a wallet must
// not cost a subscription.
var reasons = map[openrails.DeclineReason]reasonSpec{
	openrails.DeclineGeneric:           {category: IssuerSoft, action: Retry},
	openrails.DeclineDoNotHonor:        {category: IssuerSoft, action: Retry},
	openrails.DeclineInsufficientFunds: {category: IssuerSoft, action: Retry},
	openrails.DeclineOverLimit:         {category: IssuerSoft, action: Retry},
	openrails.DeclineCallIssuer:        {category: IssuerSoft, action: Retry},
	openrails.DeclineRetryLater:        {category: IssuerSoft, action: Retry},
	openrails.DeclineTryAgainLater:     {category: IssuerSoft, action: Retry, transient: true},
	openrails.DeclineSecurityViolation: {category: IssuerHard, action: Retry},
	openrails.DeclineRestrictedCard:    {category: IssuerHard, action: Retry},

	openrails.DeclineIncorrectNumber:      {category: CardData, action: FixPaymentMethod},
	openrails.DeclineNoSuchIssuer:         {category: CardData, action: FixPaymentMethod},
	openrails.DeclineInvalidAccount:       {category: CardData, action: FixPaymentMethod},
	openrails.DeclineExpiredCard:          {category: CardData, action: FixPaymentMethod},
	openrails.DeclineInvalidExpiry:        {category: CardData, action: FixPaymentMethod},
	openrails.DeclineIncorrectCVC:         {category: CardData, action: FixPaymentMethod},
	openrails.DeclineIncorrectZip:         {category: CardData, action: FixPaymentMethod},
	openrails.DeclineIncorrectAddress:     {category: CardData, action: FixPaymentMethod},
	openrails.DeclineInvalidPIN:           {category: CardData, action: FixPaymentMethod},
	openrails.DeclineUpdateCardholderData: {category: CardData, action: FixPaymentMethod},

	openrails.DeclineTransactionNotAllowed:  {category: IssuerHard, action: FixPaymentMethod},
	openrails.DeclineCardNotSupported:       {category: IssuerHard, action: FixPaymentMethod},
	openrails.DeclineCurrencyNotSupported:   {category: IssuerHard, action: FixPaymentMethod},
	openrails.DeclineAuthenticationRequired: {category: IssuerHard, action: FixPaymentMethod},
	openrails.DeclinePickupCard:             {category: IssuerHard, action: FixPaymentMethod},
	openrails.DeclineLostCard:               {category: IssuerHard, action: FixPaymentMethod},
	openrails.DeclineStolenCard:             {category: IssuerHard, action: NonRecoverable},
	openrails.DeclineFraudulent:             {category: IssuerHard, action: NonRecoverable},
	openrails.DeclineStopRecurring:          {category: IssuerHard, action: NonRecoverable},

	openrails.DeclineGatewayRejected: {category: GatewayRule, action: Retry},
	openrails.DeclineBlockedByPSP:    {category: GatewayRule, action: FixPaymentMethod},

	openrails.DeclineProcessingError:      {category: SystemError, action: Retry, transient: true},
	openrails.DeclineCommunicationError:   {category: SystemError, action: Retry, transient: true},
	openrails.DeclineIssuerUnavailable:    {category: SystemError, action: Retry, transient: true},
	openrails.DeclineInvalidRequest:       {category: SystemError, action: Retry},
	openrails.DeclineMerchantConfig:       {category: SystemError, action: Retry},
	openrails.DeclineDuplicateTransaction: {category: SystemError, action: Retry},

	openrails.DeclineUnknown: {category: Unknown, action: Retry},
}

// ProviderFault reports a refusal another card cannot fix: the gateway, the
// processor or our configuration refused it.
func ProviderFault(reason openrails.DeclineReason) bool {
	return (reasons[reason].category == SystemError && reason != openrails.DeclineDuplicateTransaction) || reason == openrails.DeclineGatewayRejected
}
