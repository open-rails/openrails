package billing

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

// NetworkNotification is a customer's billing notice at an actively linked
// merchant. Reading it uses the same inbox state as that merchant's /me API.
type NetworkNotification struct {
	LinkID       CustomerLinkID `json:"link_id"`
	MerchantID   MerchantID     `json:"merchant_id"`
	Merchant     PublicMerchant `json:"merchant"`
	Notification Notification   `json:"notification"`
}
