package billing

import "time"

// NetworkLink is one of the openrails.dev account's active links, with its
// merchant as that merchant's customers see it.
type NetworkLink struct {
	Link     CustomerLink   `json:"link"`
	Merchant PublicMerchant `json:"merchant"`
}

// NetworkSubscription is one of the account's subscriptions at a linked
// merchant, as that merchant's customer reads it, and the link it acts
// through.
type NetworkSubscription struct {
	LinkID       CustomerLinkID `json:"link_id"`
	MerchantID   MerchantID     `json:"merchant_id"`
	Subscription Subscription   `json:"subscription"`
}

// NetworkOrder is one of the account's orders at a linked merchant, as that
// merchant's customer reads it, and the link it acts through.
type NetworkOrder struct {
	LinkID     CustomerLinkID `json:"link_id"`
	MerchantID MerchantID     `json:"merchant_id"`
	Order      Order          `json:"order"`
}

// PaymentAuthorityRevocation is the customer ending a merchant's authority
// to charge its cards without it: every live agreement ends (status revoked,
// customer_revoked). Subscriptions are not canceled; a renewal waits for the
// customer to pay it.
type PaymentAuthorityRevocation struct {
	MerchantID      MerchantID  `json:"merchant_id"`
	CustomerID      CustomerID  `json:"customer_id"`
	RevokedMandates []MandateID `json:"revoked_mandates"`
	RevokedAt       time.Time   `json:"revoked_at"`
}

// PaymentAuthorityHostEvent is customer.payment_authority_revoked.
type PaymentAuthorityHostEvent struct {
	CustomerID      CustomerID  `json:"customer_id"`
	RevokedMandates []MandateID `json:"revoked_mandates"`
}
