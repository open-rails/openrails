package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
)

// Subscription helper functions (moved from db model to service layer)

// IsExpired checks if a subscription has expired at the given time.
// Pass time.Now() for current state, or a mock time for testing.
func IsExpired(s *models.Subscription, at time.Time) bool {
	return s.CurrentPeriodEndsAt != nil && s.CurrentPeriodEndsAt.Before(at)
}

// -------------------------------- Utility Types --------------------------------

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
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			*s = ""
			return nil
		}
		if math.Trunc(v) == v {
			*s = Stringish(strconv.FormatInt(int64(v), 10))
		} else {
			*s = Stringish(strconv.FormatFloat(v, 'f', -1, 64))
		}
	case bool:
		*s = Stringish(strconv.FormatBool(v))
	default:
		*s = Stringish(fmt.Sprint(v))
	}
	return nil
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

// Float64 parses the value as a float64 when present.
func (s Stringish) Float64() (float64, error) {
	trimmed := strings.TrimSpace(string(s))
	if trimmed == "" {
		return 0, errors.New("value is empty")
	}
	return strconv.ParseFloat(trimmed, 64)
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

// -------------------------------- NMI Webhook Types --------------------------------

type NMIWebhookEvent struct {
	EventID   string              `json:"event_id" validate:"required"`
	EventType NMIWebhookEventType `json:"event_type" validate:"required"`
	EventBody json.RawMessage     `json:"event_body" validate:"required"`
}

type NMIRecurringEventBody struct {
	SubscriptionID    Stringish          `json:"subscription_id"`
	AttemptedPayments Intish             `json:"attempted_payments"`
	CompletedPayments Intish             `json:"completed_payments"`
	BillingAddress    *NMIBillingAddress `json:"billing_address"`
	Card              *NMICard           `json:"card"`
	Features          *NMIFeatures       `json:"features"`
	Merchant          *NMIMerchant       `json:"merchant"`
	NextChargeDate    Stringish          `json:"next_charge_date"`
	OrderDescription  Stringish          `json:"order_description"`
	OrderID           Stringish          `json:"order_id"`
	Plan              *NMIPlan           `json:"plan"`
	PONumber          Stringish          `json:"ponumber"`
	ProcessorID       Stringish          `json:"processor_id"`
	RemainingPayments Stringish          `json:"remaining_payments"`
	Shipping          Stringish          `json:"shipping"`
	SubscriptionType  Stringish          `json:"subscription_type"`
	Tax               Stringish          `json:"tax"`
	Website           Stringish          `json:"website"`
}

type NMITransactionEventBody struct {
	TransactionID         Stringish             `json:"transaction_id"`
	TransactionType       Stringish             `json:"transaction_type"`
	Condition             Stringish             `json:"condition"`
	Amount                Stringish             `json:"amount"`
	RequestedAmount       Stringish             `json:"requested_amount"`
	Currency              Stringish             `json:"currency"`
	OrderID               Stringish             `json:"order_id"`
	OrderDescription      Stringish             `json:"order_description"`
	PONumber              Stringish             `json:"ponumber"`
	ProcessorID           Stringish             `json:"processor_id"`
	CustomerID            Stringish             `json:"customerid"`
	CustomerTaxID         Stringish             `json:"customertaxid"`
	CustomerVaultID       Stringish             `json:"customer_vault_id"`
	Website               Stringish             `json:"website"`
	Shipping              Stringish             `json:"shipping"`
	ShippingCarrier       Stringish             `json:"shipping_carrier"`
	TrackingNumber        Stringish             `json:"tracking_number"`
	ShippingDate          Stringish             `json:"shipping_date"`
	Tax                   Stringish             `json:"tax"`
	Surcharge             Stringish             `json:"surcharge"`
	ConvenienceFee        Stringish             `json:"convenience_fee"`
	MiscFee               Stringish             `json:"misc_fee"`
	MiscFeeName           Stringish             `json:"misc_fee_name"`
	CashDiscount          Stringish             `json:"cash_discount"`
	Tip                   Stringish             `json:"tip"`
	PartialPaymentID      Stringish             `json:"partial_payment_id"`
	PartialPaymentBalance Stringish             `json:"partial_payment_balance"`
	PlatformID            Stringish             `json:"platform_id"`
	AuthorizationCode     Stringish             `json:"authorization_code"`
	SocialSecurityNumber  Stringish             `json:"social_security_number"`
	DriversLicenseNumber  Stringish             `json:"drivers_license_number"`
	DriversLicenseState   Stringish             `json:"drivers_license_state"`
	DriversLicenseDOB     Stringish             `json:"drivers_license_dob"`
	Merchant              *NMIMerchant          `json:"merchant"`
	Features              *NMIFeatures          `json:"features"`
	Subscription          *NMISubscriptionRef   `json:"subscription"`
	Action                *NMIAction            `json:"action"`
	TransactionDetail     *NMITransactionDetail `json:"transaction"`
	BillingAddress        *NMIBillingAddress    `json:"billing_address"`
	ShippingAddress       *NMIBillingAddress    `json:"shipping_address"`
	Card                  *NMICard              `json:"card"`
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

type NMIACUEventBody struct {
	VaultID       Stringish           `json:"vault_id"`
	CustomerID    Stringish           `json:"customer_id"`
	Subscription  *NMISubscriptionRef `json:"subscription"`
	PaymentMethod *NMIPaymentInfo     `json:"payment_method"`
}

// NMIChargebackBatchEventBody represents NMI's chargeback.batch.complete webhook payload
// Note: NMI chargeback webhooks are batch-based and do NOT include transaction_id or subscription info
// This makes automatic subscription termination impossible without additional API lookups
type NMIChargebackBatchEventBody struct {
	Merchant         *NMIMerchant         `json:"merchant"`
	Processor        *NMIProcessorRef     `json:"processor"`
	Batch            *NMIChargebackBatch  `json:"batch"`
	Count            int                  `json:"count"`
	ChargebackAmount string               `json:"chargeback_amount"`
	Chargebacks      []NMIChargebackEntry `json:"chargebacks"`
}

type NMIProcessorRef struct {
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
	Amount                        Stringish `json:"amount"`
	ActionType                    string    `json:"action_type"`
	Date                          string    `json:"date"`
	Success                       Stringish `json:"success"`
	IPAddress                     string    `json:"ip_address"`
	Source                        string    `json:"source"`
	APIMethod                     string    `json:"api_method"`
	Username                      string    `json:"username"`
	Response                      Stringish `json:"response"`
	ResponseCode                  Stringish `json:"response_code"`
	ResponseText                  string    `json:"response_text"`
	ProcessorResponseText         string    `json:"processor_response_text"`
	ProcessorResponseCode         string    `json:"processor_response_code"`
	NetworkTokenUsed              bool      `json:"network_token_used"`
	NetworkTokenCryptogramCreated bool      `json:"network_token_cryptogram_created"`
	DeviceLicenseNumber           string    `json:"device_license_number"`
	DeviceNickname                string    `json:"device_nickname"`
	Type                          string    `json:"type"`
}

// NMIPaymentInfo represents updated payment method information from ACU
type NMIPaymentInfo struct {
	LastFour   Stringish `json:"last_four"`
	CardType   Stringish `json:"card_type"`
	ExpiryDate Stringish `json:"expiry_date"`
}

type NMIBillingAddress struct {
	Address1   string `json:"address_1"`
	Address2   string `json:"address_2"`
	CellPhone  string `json:"cell_phone"`
	City       string `json:"city"`
	Company    string `json:"company"`
	Country    string `json:"country"`
	Email      string `json:"email" validate:"required,email"`
	Fax        string `json:"fax"`
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
	Phone      string `json:"phone"`
	PostalCode string `json:"postal_code"`
	State      string `json:"state"`
}

type NMICard struct {
	AVSResponse          string `json:"avs_response"`
	CardAvailableBalance string `json:"card_available_balance"`
	CardBalance          string `json:"card_balance"`
	CardholderAuth       string `json:"cardholder_auth"`
	CAVV                 string `json:"cavv"`
	CAVVResult           string `json:"cavv_result"`
	CCBin                string `json:"cc_bin"`
	CCExp                string `json:"cc_exp"`
	CCIssueNumber        string `json:"cc_issue_number"`
	CCNumber             string `json:"cc_number"`
	CCStartDate          string `json:"cc_start_date"`
	CCType               string `json:"cc_type"`
	CSCResponse          string `json:"csc_response"`
	ECI                  string `json:"eci"`
	EntryMode            string `json:"entry_mode"`
	XID                  string `json:"xid"`
}

type NMIMerchant struct {
	ID   Stringish `json:"id"`
	Name string    `json:"name"`
}

type NMIPlan struct {
	Name           string    `json:"name"`
	Amount         Stringish `json:"amount"`
	Payments       Stringish `json:"payments"`
	DayOfMonth     *Intish   `json:"day_of_month"`
	DayFrequency   *Intish   `json:"day_frequency"`
	MonthFrequency *Intish   `json:"month_frequency"`
	ID             Stringish `json:"id"`
}

type NMIFeatures struct {
	IsTestMode bool `json:"is_test_mode"`
}

// -------------------------------- CCBill Webhook Types --------------------------------

type CCBillWebhookEvent struct {
	EventType CCBillWebhookEventType
	EventBody []byte
	Version   string // Detected or provided webhook version
}

// CCBillWebhookVersion represents supported CCBill webhook payload versions
type CCBillWebhookVersion string

const (
	CCBillVersionV2 CCBillWebhookVersion = "v2"
	CCBillVersionV4 CCBillWebhookVersion = "v4"
	CCBillVersionV5 CCBillWebhookVersion = "v5"
	CCBillVersionV7 CCBillWebhookVersion = "v7"
	CCBillVersionV8 CCBillWebhookVersion = "v8"
)

// CCBillVersionedPayload represents a versioned webhook payload that can be parsed
type CCBillVersionedPayload interface {
	GetVersion() CCBillWebhookVersion
	GetTransactionID() string
	GetSubscriptionID() string
	GetClientAccnum() string
	GetClientSubacc() string
	GetTimestamp() string
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

// Implement CCBillVersionedPayload interface
func (e CCBillNewSaleSuccessEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV8 }
func (e CCBillNewSaleSuccessEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillNewSaleSuccessEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillNewSaleSuccessEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillNewSaleSuccessEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillNewSaleSuccessEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillUpgradeSuccessEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV5 }
func (e CCBillUpgradeSuccessEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillUpgradeSuccessEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillUpgradeSuccessEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillUpgradeSuccessEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillUpgradeSuccessEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillUpgradeFailureEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV4 }
func (e CCBillUpgradeFailureEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillUpgradeFailureEvent) GetSubscriptionID() string        { return "" } // UpgradeFailure may not have subscriptionId
func (e CCBillUpgradeFailureEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillUpgradeFailureEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillUpgradeFailureEvent) GetTimestamp() string             { return e.Timestamp }

// CCBillBillingDateChangeEvent represents official CCBill BillingDateChange webhook v2 (Feb 2025)
type CCBillBillingDateChangeEvent struct {
	// Core fields
	SubscriptionID  string `json:"subscriptionId" validate:"required"`
	ClientAccnum    string `json:"clientAccnum" validate:"required"`
	ClientSubacc    string `json:"clientSubacc" validate:"required"`
	Timestamp       string `json:"timestamp" validate:"required"`
	NextRenewalDate string `json:"nextRenewalDate" validate:"required"`
}

// Implement CCBillVersionedPayload interface
func (e CCBillBillingDateChangeEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV2 }
func (e CCBillBillingDateChangeEvent) GetTransactionID() string         { return "" }
func (e CCBillBillingDateChangeEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillBillingDateChangeEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillBillingDateChangeEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillBillingDateChangeEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillCustomerDataUpdateEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV5 }
func (e CCBillCustomerDataUpdateEvent) GetTransactionID() string         { return "" }
func (e CCBillCustomerDataUpdateEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillCustomerDataUpdateEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillCustomerDataUpdateEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillCustomerDataUpdateEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillUserReactivationEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV2 }
func (e CCBillUserReactivationEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillUserReactivationEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillUserReactivationEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillUserReactivationEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillUserReactivationEvent) GetTimestamp() string             { return "" } // UserReactivation doesn't have timestamp field

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

// Implement CCBillVersionedPayload interface
func (e CCBillRenewalSuccessEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV7 }
func (e CCBillRenewalSuccessEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillRenewalSuccessEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillRenewalSuccessEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillRenewalSuccessEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillRenewalSuccessEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillRenewalFailureEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV5 }
func (e CCBillRenewalFailureEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillRenewalFailureEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillRenewalFailureEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillRenewalFailureEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillRenewalFailureEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillNewSaleFailureEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV5 }
func (e CCBillNewSaleFailureEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillNewSaleFailureEvent) GetSubscriptionID() string        { return "" } // No subscription for failures
func (e CCBillNewSaleFailureEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillNewSaleFailureEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillNewSaleFailureEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillRefundEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV5 }
func (e CCBillRefundEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillRefundEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillRefundEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillRefundEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillRefundEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillChargebackEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV5 }
func (e CCBillChargebackEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillChargebackEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillChargebackEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillChargebackEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillChargebackEvent) GetTimestamp() string             { return e.Timestamp }

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

// Implement CCBillVersionedPayload interface
func (e CCBillVoidEvent) GetVersion() CCBillWebhookVersion { return CCBillVersionV5 }
func (e CCBillVoidEvent) GetTransactionID() string         { return e.TransactionID }
func (e CCBillVoidEvent) GetSubscriptionID() string        { return e.SubscriptionID }
func (e CCBillVoidEvent) GetClientAccnum() string          { return e.ClientAccnum }
func (e CCBillVoidEvent) GetClientSubacc() string          { return e.ClientSubacc }
func (e CCBillVoidEvent) GetTimestamp() string             { return e.Timestamp }
