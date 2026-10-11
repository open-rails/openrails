package billing

import "time"

// CustomerTokenParams names the customer a merchant's backend vouches for
// when it mints a customer token (Client.CustomerToken).
type CustomerTokenParams struct {
	// CustomerID is the customer: the token's subject.
	CustomerID CustomerID
	// Email is the customer's contact address; "" sends none.
	Email string
	// EmailVerified says the backend verified Email. The server keeps an
	// email only when it is verified.
	EmailVerified bool
}

// CustomerToken is a customer's access token: the Bearer a frontend sends to
// the customer routes (/v1/me), bound to one customer of one merchant.
type CustomerToken struct {
	AccessToken string
	// TokenType is Bearer.
	TokenType string
	// ExpiresAt is when the token stops working; mint another before it.
	ExpiresAt time.Time
}
