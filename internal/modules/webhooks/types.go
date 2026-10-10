package webhooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Stringish handles inconsistent NMI payload encoding where identifiers might be
// transmitted as strings or bare numbers.
type Stringish string

// UnmarshalJSON normalises string/number/null payloads into Stringish
func (s *Stringish) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if string(data) == "null" {
		*s = ""
		return nil
	}
	if data[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		*s = Stringish(str)
		return nil
	}
	// UseNumber keeps a bare JSON number as its literal digits: decoding into
	// `any` yields float64, which mangles an id past 2^53 and would route a
	// webhook to the wrong subscription. Amounts never round-trip through float.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case json.Number:
		*s = Stringish(trimTrailingZeroFraction(v.String()))
	case bool:
		*s = Stringish(strconv.FormatBool(v))
	default:
		*s = Stringish(fmt.Sprint(v))
	}
	return nil
}

// trimTrailingZeroFraction normalises "12.0" to "12" so a number-encoded
// identifier compares equal however the rail wrote it. Purely textual — the
// digits are never reinterpreted through a float.
func trimTrailingZeroFraction(text string) string {
	dot := strings.IndexByte(text, '.')
	if dot < 0 || strings.ContainsAny(text, "eE") {
		return text
	}
	if strings.Trim(text[dot+1:], "0") != "" {
		return text
	}
	return text[:dot]
}

// String returns the raw string value.
func (s Stringish) String() string {
	return string(s)
}

// Trimmed returns the value without surrounding whitespace.
func (s Stringish) Trimmed() string {
	return strings.TrimSpace(string(s))
}

// IsEmpty reports whether the value is blank after trimming.
func (s Stringish) IsEmpty() bool {
	return strings.TrimSpace(string(s)) == ""
}

// Intish models integer-like fields that may arrive as strings or numbers.
type Intish int

// UnmarshalJSON normalises string/number/null payloads into Intish.
func (i *Intish) UnmarshalJSON(data []byte) error {
	if i == nil {
		return errors.New("Intish: UnmarshalJSON on nil receiver")
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*i = 0
		return nil
	}
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*i = 0
			return nil
		}
		val, err := strconv.ParseInt(str, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid Intish value %q: %w", str, err)
		}
		*i = Intish(val)
		return nil
	}
	val, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid Intish value %q: %w", trimmed, err)
	}
	*i = Intish(val)
	return nil
}

// Int returns the native int value.
func (i Intish) Int() int {
	return int(i)
}

type NMIWebhookEvent struct {
	EventID   string              `json:"event_id" validate:"required"`
	EventType NMIWebhookEventType `json:"event_type" validate:"required"`
	EventBody json.RawMessage     `json:"event_body" validate:"required"`
}

// NMI webhooks are wake-ups: OpenRails reads what happened from the Query API,
// so a body carries only the references that find it.
type NMIRecurringEventBody struct {
	SubscriptionID Stringish `json:"subscription_id"`
}

type NMITransactionEventBody struct {
	TransactionID     Stringish             `json:"transaction_id"`
	Amount            Stringish             `json:"amount"`
	Currency          Stringish             `json:"currency"`
	OrderID           Stringish             `json:"order_id"`
	PONumber          Stringish             `json:"ponumber"`
	CustomerID        Stringish             `json:"customerid"`
	CustomerVaultID   Stringish             `json:"customer_vault_id"`
	Subscription      *NMISubscriptionRef   `json:"subscription"`
	Action            *NMIAction            `json:"action"`
	TransactionDetail *NMITransactionDetail `json:"transaction"`
}

type NMITransactionDetail struct {
	TransactionID   Stringish           `json:"transaction_id"`
	Amount          Stringish           `json:"amount"`
	Currency        Stringish           `json:"currency"`
	OrderID         Stringish           `json:"order_id"`
	PONumber        Stringish           `json:"ponumber"`
	CustomerID      Stringish           `json:"customerid"`
	CustomerVaultID Stringish           `json:"customer_vault_id"`
	Subscription    *NMISubscriptionRef `json:"subscription"`
	Action          *NMIAction          `json:"action"`
}

// NMIACUEventBody is an Account Updater notice. It is a wake-up: only the
// vault it names is read; the card comes from NMI's vault.
type NMIACUEventBody struct {
	VaultID         Stringish `json:"vault_id"`
	CustomerVaultID Stringish `json:"customer_vault_id"`
}

// Vault is the vault the notice is about.
func (b NMIACUEventBody) Vault() string {
	if v := b.VaultID.Trimmed(); v != "" {
		return v
	}
	return b.CustomerVaultID.Trimmed()
}

// NMIChargebackBatchEventBody is NMI's chargeback.batch.complete payload. It
// carries no transaction or subscription id, so each entry is matched to a
// charge by card last4, amount and date.
type NMIChargebackBatchEventBody struct {
	Merchant         *NMIMerchant         `json:"merchant"`
	Rail             *NMIRailRef          `json:"processor"`
	Batch            *NMIChargebackBatch  `json:"batch"`
	Count            int                  `json:"count"`
	ChargebackAmount string               `json:"chargeback_amount"`
	Chargebacks      []NMIChargebackEntry `json:"chargebacks"`
}

type NMIRailRef struct {
	ID   Stringish `json:"id"`
	Name Stringish `json:"name"`
	Type Stringish `json:"type"`
}

type NMIChargebackBatch struct {
	Count       int    `json:"count"`
	TotalAmount string `json:"total_amount"`
}

type NMIChargebackEntry struct {
	ID           Stringish `json:"id"`
	Date         string    `json:"date"`
	CustomerName string    `json:"customer_name"`
	CCNumber     string    `json:"cc_number"` // Last 4 digits masked
	Amount       string    `json:"amount"`
	ReasonCode   string    `json:"reason_code"`
	Reason       string    `json:"reason"`
}

type NMISubscriptionRef struct {
	SubscriptionID Stringish `json:"subscription_id"`
	PlanID         Stringish `json:"plan_id"`
}

type NMIAction struct {
	Amount Stringish `json:"amount"`
}

type NMIMerchant struct {
	ID   Stringish `json:"id"`
	Name string    `json:"name"`
}

type CCBillWebhookEvent struct {
	EventType CCBillWebhookEventType
	EventBody []byte
	Version   string // Detected or provided webhook version
}

type CCBillCommonFields struct {
	ClientAccnum   Stringish `json:"clientAccnum" validate:"required"`
	ClientSubacc   string    `json:"clientSubacc" validate:"required"`
	SubscriptionID string    `json:"subscriptionId" validate:"required"`
	Timestamp      string    `json:"timestamp" validate:"required"`
}

// CCBillNewSaleSuccessEvent represents official CCBill NewSaleSuccess webhook v8 (April 2025)
type CCBillNewSaleSuccessEvent struct {
	// Core transaction fields
	SubscriptionID string `json:"subscriptionId" validate:"required"`
	TransactionID  string `json:"transactionId" validate:"required"`
	ClientAccnum   string `json:"clientAccnum" validate:"required"`
	ClientSubacc   string `json:"clientSubacc" validate:"required"`
	Timestamp      string `json:"timestamp" validate:"required"`

	// Customer information
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Address1    string `json:"address1"`
	City        string `json:"city"`
	State       string `json:"state"`
	Country     string `json:"country"`
	PostalCode  string `json:"postalCode"`
	Email       string `json:"email" validate:"required,email"`
	PhoneNumber string `json:"phoneNumber"`
	IPAddress   string `json:"ipAddress"`
	Username    string `json:"username"`
	Password    string `json:"password"`

	// Product information
	FormName                  string `json:"formName"`
	FlexID                    string `json:"flexId"`
	ProductDesc               string `json:"productDesc"`
	PriceDescription          string `json:"priceDescription"`
	RecurringPriceDescription string `json:"recurringPriceDescription"`

	// Pricing information
	BilledInitialPrice         string    `json:"billedInitialPrice"`
	BilledRecurringPrice       string    `json:"billedRecurringPrice"`
	BilledCurrencyCode         Stringish `json:"billedCurrencyCode"`
	SubscriptionInitialPrice   string    `json:"subscriptionInitialPrice"`
	SubscriptionRecurringPrice string    `json:"subscriptionRecurringPrice"`
	SubscriptionCurrencyCode   Stringish `json:"subscriptionCurrencyCode"`
	AccountingInitialPrice     string    `json:"accountingInitialPrice"`
	AccountingRecurringPrice   string    `json:"accountingRecurringPrice"`
	AccountingCurrencyCode     Stringish `json:"accountingCurrencyCode"`

	// Subscription details
	InitialPeriod      Stringish `json:"initialPeriod"`
	RecurringPeriod    Stringish `json:"recurringPeriod"`
	Rebills            Stringish `json:"rebills"`
	NextRenewalDate    string    `json:"nextRenewalDate"`
	SubscriptionTypeID string    `json:"subscriptionTypeId"`

	// Payment information
	PaymentType    string `json:"paymentType"`
	CardType       string `json:"cardType"`
	Bin            string `json:"bin"`
	PrePaid        string `json:"prePaid"`
	Last4          string `json:"last4"`
	ExpDate        string `json:"expDate"`
	AVSResponse    string `json:"avsResponse"`
	CVV2Response   string `json:"cvv2Response"`
	PaymentAccount string `json:"paymentAccount"`
	ThreeDSecure   string `json:"threeDSecure"`
	CardSubType    string `json:"cardSubType"`

	// Additional fields
	ReservationID                  string    `json:"reservationId"`
	DynamicPricingValidationDigest string    `json:"dynamicPricingValidationDigest"`
	AffiliateSystem                string    `json:"affiliateSystem"`
	ReferringURL                   string    `json:"referringUrl"`
	LifeTimeSubscription           Stringish `json:"lifeTimeSubscription"`
	LifeTimePrice                  string    `json:"lifeTimePrice"`
}

// CCBillUpgradeSuccessEvent represents official CCBill UpgradeSuccess webhook v5 (April 2025)
// Contains all NewSaleSuccess v8 fields plus upgrade-specific fields
type CCBillUpgradeSuccessEvent struct {
	// Core transaction fields
	SubscriptionID string `json:"subscriptionId" validate:"required"`
	TransactionID  string `json:"transactionId" validate:"required"`
	ClientAccnum   string `json:"clientAccnum" validate:"required"`
	ClientSubacc   string `json:"clientSubacc" validate:"required"`
	Timestamp      string `json:"timestamp" validate:"required"`

	// Customer information
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Address1    string `json:"address1"`
	City        string `json:"city"`
	State       string `json:"state"`
	Country     string `json:"country"`
	PostalCode  string `json:"postalCode"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phoneNumber"`
	IPAddress   string `json:"ipAddress"`

	// Account and form information
	ReservationID string `json:"reservationId"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	FormName      string `json:"formName"`
	FlexID        string `json:"flexId"`

	// Product information
	ProductDesc               string `json:"productDesc"`
	PriceDescription          string `json:"priceDescription"`
	RecurringPriceDescription string `json:"recurringPriceDescription"`

	// Billing information
	BilledInitialPrice   string    `json:"billedInitialPrice"`
	BilledRecurringPrice string    `json:"billedRecurringPrice"`
	BilledCurrencyCode   Stringish `json:"billedCurrencyCode"`

	// Subscription information
	SubscriptionInitialPrice   string    `json:"subscriptionInitialPrice"`
	SubscriptionRecurringPrice string    `json:"subscriptionRecurringPrice"`
	SubscriptionCurrencyCode   Stringish `json:"subscriptionCurrencyCode"`

	// Accounting information
	AccountingInitialPrice   Stringish `json:"accountingInitialPrice"`
	AccountingRecurringPrice Stringish `json:"accountingRecurringPrice"`
	AccountingCurrencyCode   Stringish `json:"accountingCurrencyCode"`

	// Subscription terms
	InitialPeriod      Stringish `json:"initialPeriod"`
	RecurringPeriod    Stringish `json:"recurringPeriod"`
	Rebills            Stringish `json:"rebills"`
	NextRenewalDate    string    `json:"nextRenewalDate"`
	SubscriptionTypeID string    `json:"subscriptionTypeId"`

	// Security and validation
	DynamicPricingValidationDigest string `json:"dynamicPricingValidationDigest"`

	// Payment information
	PaymentType    string `json:"paymentType"`
	CardType       string `json:"cardType"`
	Bin            string `json:"bin"`
	PrePaid        string `json:"prePaid"`
	Last4          string `json:"last4"`
	ExpDate        string `json:"expDate"`
	AVSResponse    string `json:"avsResponse"`
	CVV2Response   string `json:"cvv2Response"`
	PaymentAccount string `json:"paymentAccount"`
	ThreeDSecure   string `json:"threeDSecure"`
	CardSubType    string `json:"cardSubType"`

	// Additional information
	AffiliateSystem      string    `json:"affiliateSystem"`
	ReferringURL         string    `json:"referringUrl"`
	LifeTimeSubscription Stringish `json:"lifeTimeSubscription"`
	LifeTimePrice        string    `json:"lifeTimePrice"`

	// -------- UpgradeSuccess-only fields (v5) --------
	OriginalSubscriptionID string    `json:"originalSubscriptionId"`
	OriginalClientAccnum   Stringish `json:"originalClientAccnum"`
	OriginalClientSubacc   string    `json:"originalClientSubacc"`
	Source                 string    `json:"source"`            // FORM | API | PHONE
	SCAResponseStatus      string    `json:"scaResponseStatus"` // E | Y | N | A | U | R

}

// CCBillUpgradeFailureEvent represents official CCBill UpgradeFailure webhook v4 (April 2025)
// Contains all NewSaleFailure fields plus upgrade-specific fields
type CCBillUpgradeFailureEvent struct {
	// Core transaction fields
	TransactionID string `json:"transactionId" validate:"required"`
	ClientAccnum  string `json:"clientAccnum" validate:"required"`
	ClientSubacc  string `json:"clientSubacc" validate:"required"`
	Timestamp     string `json:"timestamp" validate:"required"`

	// Customer information
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Address1    string `json:"address1"`
	City        string `json:"city"`
	State       string `json:"state"`
	Country     string `json:"country"`
	PostalCode  string `json:"postalCode"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phoneNumber"`
	IPAddress   string `json:"ipAddress"`

	// Account and form information
	ReservationID string `json:"reservationId"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	FormName      string `json:"formName"`
	FlexID        string `json:"flexId"`

	// Product information
	PriceDescription          string `json:"priceDescription"`
	RecurringPriceDescription string `json:"recurringPriceDescription"`

	// Billing information
	BilledInitialPrice   string    `json:"billedInitialPrice"`
	BilledRecurringPrice string    `json:"billedRecurringPrice"`
	BilledCurrencyCode   Stringish `json:"billedCurrencyCode"`

	// Subscription information
	SubscriptionInitialPrice   string    `json:"subscriptionInitialPrice"`
	SubscriptionRecurringPrice string    `json:"subscriptionRecurringPrice"`
	SubscriptionCurrencyCode   Stringish `json:"subscriptionCurrencyCode"`

	// Accounting information
	AccountingInitialPrice   string    `json:"accountingInitialPrice"`
	AccountingRecurringPrice string    `json:"accountingRecurringPrice"`
	AccountingCurrencyCode   Stringish `json:"accountingCurrencyCode"`

	// Subscription terms
	InitialPeriod      Stringish `json:"initialPeriod"`
	RecurringPeriod    Stringish `json:"recurringPeriod"`
	Rebills            Stringish `json:"rebills"`
	SubscriptionTypeID string    `json:"subscriptionTypeId"`

	// Security and validation
	DynamicPricingValidationDigest string `json:"dynamicPricingValidationDigest"`

	// Payment information
	PaymentType    string `json:"paymentType"`
	CardType       string `json:"cardType"`
	PrePaid        string `json:"prePaid"`
	AVSResponse    string `json:"avsResponse"`
	CVV2Response   string `json:"cvv2Response"`
	PaymentAccount string `json:"paymentAccount"`
	Last4          string `json:"last4"`
	ExpDate        string `json:"expDate"`
	ThreeDSecure   string `json:"threeDSecure"`

	// Additional information
	AffiliateSystem      string    `json:"affiliateSystem"`
	ReferringURL         string    `json:"referringUrl"`
	LifeTimeSubscription Stringish `json:"lifeTimeSubscription"`
	LifeTimePrice        string    `json:"lifeTimePrice"`

	// Failure information
	FailureReason string `json:"failureReason"`
	FailureCode   string `json:"failureCode"`

	// -------- UpgradeFailure-specific fields (v4) --------
	OriginalSubscriptionID string                 `json:"originalSubscriptionId"`
	OriginalClientAccnum   Stringish              `json:"originalClientAccnum"`
	OriginalClientSubacc   string                 `json:"originalClientSubacc"`
	Source                 string                 `json:"source"` // FORM | API | PHONE
	Bin                    Stringish              `json:"bin"`
	SCAResponseStatus      string                 `json:"scaResponseStatus"` // E | Y | N | A | U | R
	CardSubType            string                 `json:"cardSubType"`       // v4 addition
	PassThrough            map[string]interface{} `json:"passThrough"`       // Custom pass-through data
}

// CCBillBillingDateChangeEvent represents official CCBill BillingDateChange webhook v2 (Feb 2025)
type CCBillBillingDateChangeEvent struct {
	// Core fields
	SubscriptionID  string `json:"subscriptionId" validate:"required"`
	ClientAccnum    string `json:"clientAccnum" validate:"required"`
	ClientSubacc    string `json:"clientSubacc" validate:"required"`
	Timestamp       string `json:"timestamp" validate:"required"`
	NextRenewalDate string `json:"nextRenewalDate" validate:"required"`
}

// CCBillCustomerDataUpdateEvent represents official CCBill CustomerDataUpdate webhook v5 (Feb 2025)
type CCBillCustomerDataUpdateEvent struct {
	// Core fields
	SubscriptionID string `json:"subscriptionId" validate:"required"`
	ClientAccnum   string `json:"clientAccnum" validate:"required"`
	ClientSubacc   string `json:"clientSubacc" validate:"required"`
	Timestamp      string `json:"timestamp" validate:"required"`

	// Customer information
	FirstName      string `json:"firstName"`
	LastName       string `json:"lastName"`
	PaymentAccount string `json:"paymentAccount"`
	Address1       string `json:"address1"`
	City           string `json:"city"`
	State          string `json:"state"`
	Country        string `json:"country"`
	PostalCode     string `json:"postalCode"`
	Email          string `json:"email"`
	PhoneNumber    string `json:"phoneNumber"`
	IPAddress      string `json:"ipAddress"`
	ReservationID  string `json:"reservationId"`
	Username       string `json:"username"`
	Password       string `json:"password"`

	// Payment information
	PaymentType string    `json:"paymentType"`
	CardType    string    `json:"cardType"`
	Bin         Stringish `json:"bin"`
	ExpDate     string    `json:"expDate"`
}

// CCBillUserReactivationEvent represents official CCBill UserReactivation webhook v2 (Feb 2025)
type CCBillUserReactivationEvent struct {
	// Core fields
	SubscriptionID  string `json:"subscriptionId" validate:"required"`
	TransactionID   string `json:"transactionId" validate:"required"`
	Price           string `json:"price" validate:"required"`
	ClientAccnum    string `json:"clientAccnum" validate:"required"`
	ClientSubacc    string `json:"clientSubacc" validate:"required"`
	Email           string `json:"email" validate:"required"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	NextRenewalDate string `json:"nextRenewalDate"`
}

// CCBillRenewalSuccessEvent represents official CCBill RenewalSuccess webhook v7
type CCBillRenewalSuccessEvent struct {
	// Core transaction fields
	TransactionID  string `json:"transactionId" validate:"required"`
	SubscriptionID string `json:"subscriptionId" validate:"required"`
	ClientAccnum   string `json:"clientAccnum" validate:"required"`
	ClientSubacc   string `json:"clientSubacc" validate:"required"`
	Timestamp      string `json:"timestamp" validate:"required"`

	// Billing information
	BilledAmount       string    `json:"billedAmount"`
	BilledCurrency     string    `json:"billedCurrency"`
	BilledCurrencyCode Stringish `json:"billedCurrencyCode"`

	// Accounting information
	AccountingAmount       string    `json:"accountingAmount"`
	AccountingCurrency     string    `json:"accountingCurrency"`
	AccountingCurrencyCode Stringish `json:"accountingCurrencyCode"`

	// Renewal information
	NextRenewalDate string `json:"nextRenewalDate"`
	RenewalDate     string `json:"renewalDate"`

	// Payment information
	CardType       string `json:"cardType"`
	PaymentAccount string `json:"paymentAccount"`
	PaymentType    string `json:"paymentType"`
	Last4          string `json:"last4"`
	ExpDate        string `json:"expDate"`
	CardSubType    string `json:"cardSubType"`
}

type CCBillCancellationEvent struct {
	CCBillCommonFields

	Reason string `json:"reason"`
	Source string `json:"source"`
}

type CCBillExpirationEvent struct {
	CCBillCommonFields
}

// CCBillRenewalFailureEvent represents official CCBill RenewalFailure webhook v5
type CCBillRenewalFailureEvent struct {
	// Core transaction fields
	TransactionID  string `json:"transactionId" validate:"required"`
	SubscriptionID string `json:"subscriptionId" validate:"required"`
	ClientAccnum   string `json:"clientAccnum" validate:"required"`
	ClientSubacc   string `json:"clientSubacc" validate:"required"`
	Timestamp      string `json:"timestamp" validate:"required"`

	// Failure information
	FailureReason string `json:"failureReason"`
	FailureCode   string `json:"failureCode"`
	NextRetryDate string `json:"nextRetryDate"`
	RenewalDate   string `json:"renewalDate"`

	// Payment information
	CardType    string `json:"cardType"`
	PaymentType string `json:"paymentType"`
	CardSubType string `json:"cardSubType"`
}

// CCBillNewSaleFailureEvent represents official CCBill NewSaleFailure webhook v5
type CCBillNewSaleFailureEvent struct {
	// Core transaction fields
	TransactionID string `json:"transactionId" validate:"required"`
	ClientAccnum  string `json:"clientAccnum" validate:"required"`
	ClientSubacc  string `json:"clientSubacc" validate:"required"`
	Timestamp     string `json:"timestamp" validate:"required"`

	// Customer information
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Address1    string `json:"address1"`
	City        string `json:"city"`
	State       string `json:"state"`
	Country     string `json:"country"`
	PostalCode  string `json:"postalCode"`
	Email       string `json:"email" validate:"required,email"`
	PhoneNumber string `json:"phoneNumber"`
	IPAddress   string `json:"ipAddress"`
	Username    string `json:"username"`
	Password    string `json:"password"`

	// Product information
	FormName                  string `json:"formName"`
	FlexID                    string `json:"flexId"`
	PriceDescription          string `json:"priceDescription"`
	RecurringPriceDescription string `json:"recurringPriceDescription"`

	// Pricing information
	BilledInitialPrice         string    `json:"billedInitialPrice"`
	BilledRecurringPrice       string    `json:"billedRecurringPrice"`
	BilledCurrencyCode         Stringish `json:"billedCurrencyCode"`
	SubscriptionInitialPrice   string    `json:"subscriptionInitialPrice"`
	SubscriptionRecurringPrice string    `json:"subscriptionRecurringPrice"`
	SubscriptionCurrencyCode   Stringish `json:"subscriptionCurrencyCode"`
	AccountingInitialPrice     string    `json:"accountingInitialPrice"`
	AccountingRecurringPrice   string    `json:"accountingRecurringPrice"`
	AccountingCurrencyCode     Stringish `json:"accountingCurrencyCode"`

	// Subscription details
	InitialPeriod      Stringish `json:"initialPeriod"`
	RecurringPeriod    Stringish `json:"recurringPeriod"`
	Rebills            Stringish `json:"rebills"`
	SubscriptionTypeID string    `json:"subscriptionTypeId"`

	// Payment information
	PaymentType    string `json:"paymentType"`
	CardType       string `json:"cardType"`
	PrePaid        string `json:"prePaid"`
	AVSResponse    string `json:"avsResponse"`
	CVV2Response   string `json:"cvv2Response"`
	PaymentAccount string `json:"paymentAccount"`
	ThreeDSecure   string `json:"threeDSecure"`
	CardSubType    string `json:"cardSubType"`

	// Failure information
	FailureReason string `json:"failureReason"`
	FailureCode   string `json:"failureCode"`

	// Additional fields
	ReservationID                  string    `json:"reservationId"`
	DynamicPricingValidationDigest string    `json:"dynamicPricingValidationDigest"`
	AffiliateSystem                string    `json:"affiliateSystem"`
	ReferringURL                   string    `json:"referringUrl"`
	LifeTimeSubscription           Stringish `json:"lifeTimeSubscription"`
	LifeTimePrice                  string    `json:"lifeTimePrice"`
}

// CCBillRefundEvent represents official CCBill Refund webhook v5
type CCBillRefundEvent struct {
	// Core transaction fields
	TransactionID  string `json:"transactionId" validate:"required"`
	SubscriptionID string `json:"subscriptionId" validate:"required"`
	ClientAccnum   string `json:"clientAccnum" validate:"required"`
	ClientSubacc   string `json:"clientSubacc" validate:"required"`
	Timestamp      string `json:"timestamp" validate:"required"`

	// Refund information
	Amount       string    `json:"amount"`
	Currency     string    `json:"currency"`
	CurrencyCode Stringish `json:"currencyCode"`
	Reason       string    `json:"reason"`

	// Accounting information
	AccountingAmount       string    `json:"accountingAmount"`
	AccountingCurrency     string    `json:"accountingCurrency"`
	AccountingCurrencyCode Stringish `json:"accountingCurrencyCode"`

	// Payment information
	CardType       string `json:"cardType"`
	PaymentAccount string `json:"paymentAccount"`
	PaymentType    string `json:"paymentType"`
	Last4          string `json:"last4"`
	ExpDate        string `json:"expDate"`
}

// CCBillChargebackEvent represents official CCBill Chargeback webhook v5
type CCBillChargebackEvent struct {
	// Core transaction fields
	TransactionID  string `json:"transactionId" validate:"required"`
	SubscriptionID string `json:"subscriptionId" validate:"required"`
	ClientAccnum   string `json:"clientAccnum" validate:"required"`
	ClientSubacc   string `json:"clientSubacc" validate:"required"`
	Timestamp      string `json:"timestamp" validate:"required"`

	// Chargeback information
	Amount       string    `json:"amount"`
	Currency     string    `json:"currency"`
	CurrencyCode Stringish `json:"currencyCode"`
	Reason       string    `json:"reason"`

	// Accounting information
	AccountingAmount       string    `json:"accountingAmount"`
	AccountingCurrency     string    `json:"accountingCurrency"`
	AccountingCurrencyCode Stringish `json:"accountingCurrencyCode"`

	// Payment information
	CardType       string `json:"cardType"`
	PaymentAccount string `json:"paymentAccount"`
	PaymentType    string `json:"paymentType"`
	Bin            string `json:"bin"`
	Last4          string `json:"last4"`
	ExpDate        string `json:"expDate"`
}

// CCBillVoidEvent represents official CCBill Void webhook v5
type CCBillVoidEvent struct {
	// Core transaction fields
	TransactionID  string `json:"transactionId" validate:"required"`
	SubscriptionID string `json:"subscriptionId" validate:"required"`
	ClientAccnum   string `json:"clientAccnum" validate:"required"`
	ClientSubacc   string `json:"clientSubacc" validate:"required"`
	Timestamp      string `json:"timestamp" validate:"required"`

	// Void information
	Amount       string    `json:"amount"`
	Currency     string    `json:"currency"`
	CurrencyCode Stringish `json:"currencyCode"`
	Reason       string    `json:"reason"`

	// Accounting information
	AccountingAmount       string    `json:"accountingAmount"`
	AccountingCurrency     string    `json:"accountingCurrency"`
	AccountingCurrencyCode Stringish `json:"accountingCurrencyCode"`

	// Payment information
	CardType       string `json:"cardType"`
	PaymentAccount string `json:"paymentAccount"`
	PaymentType    string `json:"paymentType"`
	Last4          string `json:"last4"`
	ExpDate        string `json:"expDate"`
}
