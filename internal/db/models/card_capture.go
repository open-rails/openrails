package models

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrCardCaptureBinding = errors.New("purchase capture binding is invalid")

// CardCapture is private, immutable setup authority. The temporary SDK
// authorization is retained only as an envelope bound to this engine session.
// Completion keeps token hash + permanent method identity, never the token or
// SDK authorization. Archive validation shares this exact decoder.
type CardCapture struct {
	RequestFingerprint string    `json:"request_fingerprint,omitempty"`
	MerchantID         uuid.UUID `json:"merchant_id"`
	CustomerID         uuid.UUID `json:"customer_id"`
	PSPID              uuid.UUID `json:"psp_id"`
	CustodianID        uuid.UUID `json:"custodian_id"`
	Environment        string    `json:"environment"`
	AccountID          string    `json:"account_id"`
	ProfileID          string    `json:"profile_id"`
	PublicAPIKey       string    `json:"public_api_key"`
	APIBaseURL         string    `json:"api_base_url"`
	SDKURL             string    `json:"sdk_url"`
	VendorCustomerID   string    `json:"vendor_customer_id,omitempty"`
	VendorSessionID    string    `json:"vendor_session_id,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
	SecretCiphertext   string    `json:"secret_ciphertext,omitempty"`
	PaymentMethodID    uuid.UUID `json:"payment_method_id,omitzero"`
	VendorMethodID     string    `json:"vendor_method_id,omitempty"`
	AcceptedTokenHash  string    `json:"accepted_token_hash,omitempty"`
}

func DecodeCardCapture(raw []byte, merchantID, sessionCustomerID, pspID uuid.UUID, status OrderAttemptStatus, expiry *time.Time) (CardCapture, error) {
	var capture CardCapture
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&capture) != nil || decoder.Decode(new(any)) != io.EOF {
		return capture, ErrCardCaptureBinding
	}
	if merchantID == uuid.Nil || capture.MerchantID != merchantID || capture.CustomerID != sessionCustomerID || capture.CustomerID == uuid.Nil || capture.PSPID != pspID || capture.PSPID == uuid.Nil || capture.CustodianID == uuid.Nil || expiry == nil || !capture.ExpiresAt.Equal(*expiry) {
		return capture, ErrCardCaptureBinding
	}
	for _, v := range []string{capture.AccountID, capture.ProfileID, capture.PublicAPIKey, capture.APIBaseURL, capture.SDKURL} {
		if strings.TrimSpace(v) == "" {
			return capture, ErrCardCaptureBinding
		}
	}
	if capture.Environment != "test" && capture.Environment != "live" {
		return capture, ErrCardCaptureBinding
	}
	if (capture.VendorSessionID == "") != (capture.VendorCustomerID == "") {
		return capture, ErrCardCaptureBinding
	}
	switch status {
	case OrderAttemptStatusCreated:
		if capture.VendorSessionID != "" || capture.SecretCiphertext != "" || capture.PaymentMethodID != uuid.Nil {
			return capture, ErrCardCaptureBinding
		}
	case OrderAttemptStatusRequiresAction:
		if capture.VendorSessionID == "" || capture.SecretCiphertext == "" || capture.PaymentMethodID != uuid.Nil {
			return capture, ErrCardCaptureBinding
		}
	case OrderAttemptStatusSucceeded:
		decoded, err := hex.DecodeString(capture.AcceptedTokenHash)
		if capture.SecretCiphertext != "" || capture.VendorSessionID == "" || capture.PaymentMethodID == uuid.Nil || capture.VendorMethodID == "" || err != nil || len(decoded) != 32 {
			return capture, ErrCardCaptureBinding
		}
	case OrderAttemptStatusExpired, OrderAttemptStatusCanceled, OrderAttemptStatusFailed:
		if capture.SecretCiphertext != "" || capture.PaymentMethodID != uuid.Nil {
			return capture, ErrCardCaptureBinding
		}
	default:
		return capture, ErrCardCaptureBinding
	}
	if capture.PaymentMethodID == uuid.Nil && (capture.VendorMethodID != "" || capture.AcceptedTokenHash != "") {
		return capture, ErrCardCaptureBinding
	}
	return capture, nil
}
