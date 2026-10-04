package nmi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/decline"
)

// Server card entry (#1129): the card reached OpenRails itself, so it is
// vaulted with the classic Direct Post Customer Vault call. v5 POST /customers
// is documented to take a card in payment_details but has not been probed
// against the live gateway; the classic call is the one known to work.
//
// The caller names the vault and billing entry. An outcome lost in transit is
// then settled by reading that vault, never by sending the card again: the
// card is wiped when either call returns and is recorded nowhere.

// CreateCustomerVaultFromCard vaults card as a new customer vault vaultID whose
// one billing entry is billingID. The response's BillingID and Card are empty:
// the Direct Post answer carries neither, so callers read the vault.
func (c *NMIClient) CreateCustomerVaultFromCard(ctx context.Context, vaultID, billingID string, data CreateCustomerVaultData, card *cardguard.Card) (*CreateCustomerVaultResponse, error) {
	defer card.Zero()
	out, err := c.vaultCard(ctx, "add_customer", vaultID, billingID, data, card)
	if err != nil {
		return nil, err
	}
	// The gateway's id is the vault's name; it echoes the one it was given.
	id := strings.TrimSpace(out.Get("customer_vault_id"))
	if id == "" {
		id = strings.TrimSpace(vaultID)
	}
	return &CreateCustomerVaultResponse{CustomerVaultID: id}, nil
}

// AddCustomerBillingFromCard stores card as the further billing entry
// billingID of an existing vault.
func (c *NMIClient) AddCustomerBillingFromCard(ctx context.Context, vaultID, billingID string, data CreateCustomerVaultData, card *cardguard.Card) error {
	defer card.Zero()
	_, err := c.vaultCard(ctx, "add_billing", vaultID, billingID, data, card)
	return err
}

func (c *NMIClient) vaultCard(ctx context.Context, action, vaultID, billingID string, data CreateCustomerVaultData, card *cardguard.Card) (url.Values, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	vaultID, billingID = strings.TrimSpace(vaultID), strings.TrimSpace(billingID)
	if vaultID == "" || billingID == "" {
		return nil, errors.New("customer vault ID and billing ID are required")
	}
	if strings.TrimSpace(data.PaymentToken) != "" {
		return nil, errors.New("a card and a payment token cannot be vaulted together")
	}
	form := url.Values{
		"security_key":      {c.SecurityKey},
		"customer_vault":    {action},
		"customer_vault_id": {vaultID},
		"billing_id":        {billingID},
	}
	for key, value := range map[string]string{
		"first_name": data.FirstName, "last_name": data.LastName, "company": data.Company,
		"address1": data.Address1, "address2": data.Address2, "city": data.City, "state": data.State,
		"zip": data.Zip, "country": data.Country, "phone": data.Phone, "email": data.Email,
	} {
		if value = strings.TrimSpace(value); value != "" {
			form.Set(key, value)
		}
	}
	// The card is appended as bytes, into capacity reserved up front, so the
	// one buffer that ever holds it can be wiped.
	encoded := form.Encode()
	body := make([]byte, 0, len(encoded)+cardFieldsCap)
	body = append(body, encoded...)
	defer func() { clear(body[:cap(body)]) }()
	if !cardguard.Unseal(card, func(number, cvc []byte, month, year int) {
		body = append(body, "&ccnumber="...)
		body = append(body, number...)
		body = append(body, "&ccexp="...)
		body = append(body, fmt.Sprintf("%02d%02d", month, year%100)...)
		body = append(body, "&cvv="...)
		body = append(body, cvc...)
	}) {
		return nil, errors.New("card is required")
	}

	raw, err := c.sendDirectBody(ctx, action, body)
	if err != nil {
		return nil, err
	}
	out, err := parseDirectResponse(raw)
	if err != nil {
		return nil, err
	}
	if isDirectResponseApproved(out) {
		return out, nil
	}
	// The gateway's answer is rebuilt from its parsed facts; the raw reply is
	// never kept beside a card request.
	code, _ := strconv.Atoi(strings.TrimSpace(out.Get("response_code")))
	return nil, &CustomerVaultError{
		Message:        "card was not vaulted",
		ResponseCode:   code,
		LocalizationID: decline.NMILocalizationID(code),
		Detail:         decline.NMIMessage(code),
		AVSResponse:    strings.TrimSpace(out.Get("avsresponse")),
		CVVResponse:    strings.TrimSpace(out.Get("cvvresponse")),
		TransactionID:  strings.TrimSpace(out.Get("transactionid")),
		ResponseText:   vaultRefusalText(out),
	}
}

// cardFieldsCap bounds "&ccnumber=" + 19 digits + "&ccexp=" + MMYY + "&cvv=" +
// 4 digits.
const cardFieldsCap = 64

// vaultRefusalText is the gateway's responsetext with any digit run long
// enough to be a card number removed.
func vaultRefusalText(out url.Values) string {
	text := strings.TrimSpace(out.Get("responsetext"))
	if cardguard.ContainsPAN(text) {
		return "refused"
	}
	return text
}
