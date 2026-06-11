package subscriptions

import "time"

const (
	DunningInterval    = 72 * time.Hour
	MaxDunningFailures = 5
)

// DeclineCategory classifies a processor decline for dunning purposes.
type DeclineCategory int

const (
	// DeclineSoft indicates a transient/recoverable decline (e.g. insufficient
	// funds). Dunning should keep retrying on the normal schedule.
	DeclineSoft DeclineCategory = iota
	// DeclineHard indicates a permanent decline (e.g. stolen card, do-not-honor,
	// account closed). Dunning must stop immediately and cancel the subscription;
	// retrying cannot succeed and may flag the merchant with the card networks.
	DeclineHard
)

// hardDeclineNMICodes maps NMI Payment API response_code values that represent
// permanent declines where further retries must stop immediately. These are
// card/account-level failures that cannot be cured by retrying the same stored
// payment method, so we cancel rather than burn dunning attempts (and risk
// flagging the merchant with the card networks).
//
//	201 = Do not honor
//	204 = Transaction not allowed
//	220 = Incorrect payment information
//	221 = No such card issuer
//	222 = No card number on file with issuer
//	223 = Expired card
//	224 = Invalid expiration date
//	225 = Invalid card security code
//	226 = Invalid PIN
//	240 = Call issuer for further information
//	250 = Pick up card
//	251 = Lost card
//	252 = Stolen card
//	253 = Fraudulent card
//	261 = Declined - Stop all recurring payments (account closed)
//	262 = Declined - Stop this recurring program
//	263 = Declined - Update cardholder data available (stored data no longer valid)
//	461 = Unsupported card type
//
// Soft (retryable) codes are intentionally NOT listed and fall through to the
// normal retry schedule:
//
//	200 = Declined by processor (generic)   202 = Insufficient funds
//	203 = Over limit                         260 = Declined w/ further instructions
//	264 = Declined - Retry in a few days     300 = Rejected by gateway
//	400 = Processor error                    410/411 = Merchant config/inactive
//	420/421 = Communication error            430 = Duplicate transaction
//	440/441 = Processor format/info error    460 = Processor feature unavailable
var hardDeclineNMICodes = map[int]bool{
	201: true,
	204: true,
	220: true,
	221: true,
	222: true,
	223: true,
	224: true,
	225: true,
	226: true,
	240: true,
	250: true,
	251: true,
	252: true,
	253: true,
	261: true,
	262: true,
	263: true,
	461: true,
}

// ClassifyNMIDecline categorizes an NMI Payment API response_code as a hard or
// soft decline. Unknown/zero codes and known transient codes (e.g. 200 generic
// decline, 202 insufficient funds, 264 retry-in-a-few-days, 300/400/420
// gateway/processor/comms errors) are treated as soft so the normal retry
// schedule applies. Merchant-side errors (410/411) are also soft so a
// configuration problem on our end does not cancel the customer's subscription.
func ClassifyNMIDecline(responseCode int) DeclineCategory {
	if hardDeclineNMICodes[responseCode] {
		return DeclineHard
	}
	return DeclineSoft
}
