package billing

// NextAction is the step the buyer takes to finish a payment that requires
// action. redirect_to_url: open URL (a Stripe or CCBill page) in the top
// window; it returns to the success URL. solana_pay: show URL (a solana: Solana
// Pay link) as a QR code or wallet link. solana_sign_transactions: the wallet
// signs and sends Transactions (base64, unsigned) in order, then the attempt is
// confirmed with each signature. authenticate: the PSP's own script completes
// Payload on the host page (Stripe.js handleNextAction with client_secret),
// then the payable is confirmed.
type NextAction struct {
	Type         string            `json:"type"`
	URL          *string           `json:"url"`
	Transactions []string          `json:"transactions"`
	PSPID        *PSPID            `json:"psp_id"`
	Payload      map[string]string `json:"payload"`
}

// PaymentFailure is the customer-facing reason a card payment was definitely
// declined. Reason is provider-neutral (incorrect_cvc, incorrect_zip,
// incorrect_address, incorrect_number, expired_card, invalid_expiry,
// insufficient_funds, over_limit, card_not_supported, currency_not_supported,
// processing_error, try_again_later, authentication_required, do_not_honor,
// generic_decline); fraud-related declines always read generic_decline.
// Message is OpenRails' copy for the buyer. Field names the card field to
// correct: cvc, postal_code, number, expiry or "". DeclineReason.Failure
// renders one.
type PaymentFailure struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}
