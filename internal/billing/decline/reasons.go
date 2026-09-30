package decline

import "slices"

// Reason is the rail-agnostic cause of a refusal, recorded as failure_reason.
type Reason string

const (
	GenericDecline    Reason = "generic_decline"
	DoNotHonor        Reason = "do_not_honor"
	InsufficientFunds Reason = "insufficient_funds"
	OverLimit         Reason = "over_limit"
	CallIssuer        Reason = "call_issuer"
	RetryLater        Reason = "retry_later"
	TryAgainLater     Reason = "try_again_later"
	SecurityViolation Reason = "security_violation"
	RestrictedCard    Reason = "restricted_card"

	IncorrectNumber      Reason = "incorrect_number"
	NoSuchIssuer         Reason = "no_such_issuer"
	InvalidAccount       Reason = "invalid_account"
	ExpiredCard          Reason = "expired_card"
	InvalidExpiry        Reason = "invalid_expiry"
	IncorrectCVC         Reason = "incorrect_cvc"
	IncorrectZip         Reason = "incorrect_zip"
	IncorrectAddress     Reason = "incorrect_address"
	InvalidPIN           Reason = "invalid_pin"
	UpdateCardholderData Reason = "update_cardholder_data"

	TransactionNotAllowed  Reason = "transaction_not_allowed"
	CardNotSupported       Reason = "card_not_supported"
	CurrencyNotSupported   Reason = "currency_not_supported"
	AuthenticationRequired Reason = "authentication_required"
	PickupCard             Reason = "pickup_card"
	LostCard               Reason = "lost_card"
	StolenCard             Reason = "stolen_card"
	Fraudulent             Reason = "fraudulent"
	StopRecurring          Reason = "stop_recurring"

	GatewayRejected Reason = "gateway_rejected"
	BlockedByPSP    Reason = "blocked_by_psp"

	ProcessingError      Reason = "processing_error"
	CommunicationError   Reason = "communication_error"
	IssuerUnavailable    Reason = "issuer_unavailable"
	InvalidRequest       Reason = "invalid_request"
	MerchantConfig       Reason = "merchant_config"
	DuplicateTransaction Reason = "duplicate_transaction"

	UnknownReason Reason = "unknown"
)

type reasonSpec struct {
	category  Category
	action    Action
	customer  string
	transient bool // the processor asked to try again shortly
	hidden    bool // fraud signals are never shown to the buyer
}

// reasons is the policy. Actions follow #1108 decision 2:
//   - retry: issuer soft/generic declines (Visa category 2/4: retries allowed)
//     and our or the gateway's errors;
//   - fix_payment_method: bad card data, and codes that forbid re-attempts on
//     the same card number (Visa category 1, Mastercard MAC 03/21); a new card
//     from the customer or the account updater resumes dunning;
//   - non_recoverable: stolen, fraud, and a revoked recurring mandate.
//
// 250/251 wait for a new card by owner decision (or#870): losing a wallet must
// not cost a subscription.
var reasons = map[Reason]reasonSpec{
	GenericDecline:    {category: IssuerSoft, action: Retry, customer: CustomerGeneric},
	DoNotHonor:        {category: IssuerSoft, action: Retry, customer: CustomerDoNotHonor},
	InsufficientFunds: {category: IssuerSoft, action: Retry, customer: CustomerInsufficientFunds},
	OverLimit:         {category: IssuerSoft, action: Retry, customer: CustomerOverLimit},
	CallIssuer:        {category: IssuerSoft, action: Retry, customer: CustomerDoNotHonor},
	RetryLater:        {category: IssuerSoft, action: Retry, customer: CustomerTryAgainLater},
	TryAgainLater:     {category: IssuerSoft, action: Retry, customer: CustomerTryAgainLater, transient: true},
	SecurityViolation: {category: IssuerHard, action: Retry, customer: CustomerGeneric, hidden: true},
	RestrictedCard:    {category: IssuerHard, action: Retry, customer: CustomerGeneric, hidden: true},

	IncorrectNumber:      {category: CardData, action: FixPaymentMethod, customer: CustomerIncorrectNumber},
	NoSuchIssuer:         {category: CardData, action: FixPaymentMethod, customer: CustomerIncorrectNumber},
	InvalidAccount:       {category: CardData, action: FixPaymentMethod, customer: CustomerIncorrectNumber},
	ExpiredCard:          {category: CardData, action: FixPaymentMethod, customer: CustomerExpiredCard},
	InvalidExpiry:        {category: CardData, action: FixPaymentMethod, customer: CustomerInvalidExpiry},
	IncorrectCVC:         {category: CardData, action: FixPaymentMethod, customer: CustomerIncorrectCVC},
	IncorrectZip:         {category: CardData, action: FixPaymentMethod, customer: CustomerIncorrectZip},
	IncorrectAddress:     {category: CardData, action: FixPaymentMethod, customer: CustomerIncorrectAddress},
	InvalidPIN:           {category: CardData, action: FixPaymentMethod, customer: CustomerGeneric},
	UpdateCardholderData: {category: CardData, action: FixPaymentMethod, customer: CustomerDoNotHonor},

	TransactionNotAllowed:  {category: IssuerHard, action: FixPaymentMethod, customer: CustomerCardNotSupported},
	CardNotSupported:       {category: IssuerHard, action: FixPaymentMethod, customer: CustomerCardNotSupported},
	CurrencyNotSupported:   {category: IssuerHard, action: FixPaymentMethod, customer: CustomerCurrencyNotSupported},
	AuthenticationRequired: {category: IssuerHard, action: FixPaymentMethod, customer: CustomerAuthenticationRequired},
	PickupCard:             {category: IssuerHard, action: FixPaymentMethod, customer: CustomerGeneric, hidden: true},
	LostCard:               {category: IssuerHard, action: FixPaymentMethod, customer: CustomerGeneric, hidden: true},
	StolenCard:             {category: IssuerHard, action: NonRecoverable, customer: CustomerGeneric, hidden: true},
	Fraudulent:             {category: IssuerHard, action: NonRecoverable, customer: CustomerGeneric, hidden: true},
	StopRecurring:          {category: IssuerHard, action: NonRecoverable, customer: CustomerDoNotHonor},

	GatewayRejected: {category: GatewayRule, action: Retry, customer: CustomerProcessingError},
	BlockedByPSP:    {category: GatewayRule, action: FixPaymentMethod, customer: CustomerGeneric, hidden: true},

	ProcessingError:      {category: SystemError, action: Retry, customer: CustomerProcessingError, transient: true},
	CommunicationError:   {category: SystemError, action: Retry, customer: CustomerTryAgainLater, transient: true},
	IssuerUnavailable:    {category: SystemError, action: Retry, customer: CustomerTryAgainLater, transient: true},
	InvalidRequest:       {category: SystemError, action: Retry, customer: CustomerProcessingError},
	MerchantConfig:       {category: SystemError, action: Retry, customer: CustomerProcessingError},
	DuplicateTransaction: {category: SystemError, action: Retry, customer: CustomerProcessingError},

	UnknownReason: {category: Unknown, action: Retry, customer: CustomerGeneric},
}

// Reasons lists every reason, for metric dimensions.
func Reasons() []string {
	out := make([]string, 0, len(reasons))
	for r := range reasons {
		out = append(out, string(r))
	}
	slices.Sort(out)
	return out
}

// ProviderFault reports a refusal another card cannot fix: the gateway, the
// processor or our configuration refused it.
func (r Result) ProviderFault() bool {
	return (r.Category == SystemError && r.Reason != DuplicateTransaction) || r.Reason == GatewayRejected
}
