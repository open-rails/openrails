package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/doujins-org/doujins-billing/config"
)

// StripeRefundService handles Stripe refund operations
type StripeRefundService struct {
	Config *config.Config
}

// RefundParams contains parameters for creating a Stripe refund
type RefundParams struct {
	// ChargeID is the ID of the charge to refund (ch_xxx or pi_xxx for payment intents)
	ChargeID string
	// Amount in cents. If 0, refunds the full amount.
	Amount int64
	// Reason for the refund: duplicate, fraudulent, or requested_by_customer
	Reason string
}

// RefundResult contains the result of a Stripe refund
type RefundResult struct {
	ID            string `json:"id"`             // Refund ID (re_xxx)
	Amount        int64  `json:"amount"`         // Amount refunded in cents
	Currency      string `json:"currency"`       // Currency code
	ChargeID      string `json:"charge"`         // Original charge ID
	Status        string `json:"status"`         // pending, succeeded, failed, canceled
	Reason        string `json:"reason"`         // Reason for refund
	FailureReason string `json:"failure_reason"` // If failed, the reason
}

// CreateRefund creates a refund for a Stripe charge or payment intent
func (s *StripeRefundService) CreateRefund(ctx context.Context, params RefundParams) (*RefundResult, error) {
	stripeProc := s.Config.GetStripeProcessor()
	if s == nil || s.Config == nil || stripeProc == nil {
		return nil, errors.New("stripe configuration is not available")
	}
	secretKey := strings.TrimSpace(stripeProc.SecretKey)
	if secretKey == "" {
		return nil, errors.New("stripe secret key is not configured")
	}

	chargeID := strings.TrimSpace(params.ChargeID)
	if chargeID == "" {
		return nil, errors.New("charge_id or payment_intent_id is required")
	}

	values := url.Values{}

	// Stripe accepts either charge or payment_intent
	if strings.HasPrefix(chargeID, "pi_") {
		values.Set("payment_intent", chargeID)
	} else {
		values.Set("charge", chargeID)
	}

	// If amount is specified, set it (otherwise full refund)
	if params.Amount > 0 {
		values.Set("amount", strconv.FormatInt(params.Amount, 10))
	}

	// Set reason if provided
	reason := strings.TrimSpace(params.Reason)
	if reason != "" {
		// Validate reason is one of the allowed values
		switch reason {
		case "duplicate", "fraudulent", "requested_by_customer":
			values.Set("reason", reason)
		default:
			// Default to requested_by_customer for other reasons
			values.Set("reason", "requested_by_customer")
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/refunds", strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe refund request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := parseStripePortalError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe refund failed (%d)", resp.StatusCode)
		}
		return nil, errors.New(msg)
	}

	var result RefundResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse stripe refund response: %w", err)
	}

	return &result, nil
}

// GetRefund retrieves a refund by ID
func (s *StripeRefundService) GetRefund(ctx context.Context, refundID string) (*RefundResult, error) {
	stripeProc := s.Config.GetStripeProcessor()
	if s == nil || s.Config == nil || stripeProc == nil {
		return nil, errors.New("stripe configuration is not available")
	}
	secretKey := strings.TrimSpace(stripeProc.SecretKey)
	if secretKey == "" {
		return nil, errors.New("stripe secret key is not configured")
	}

	refundID = strings.TrimSpace(refundID)
	if refundID == "" {
		return nil, errors.New("refund_id is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.stripe.com/v1/refunds/"+refundID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe refund fetch failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := parseStripePortalError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe refund fetch failed (%d)", resp.StatusCode)
		}
		return nil, errors.New(msg)
	}

	var result RefundResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse stripe refund response: %w", err)
	}

	return &result, nil
}
