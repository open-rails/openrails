package subscriptions

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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

var ErrStripeRefundTargetMissing = errors.New("stripe refundable transaction id is missing")

// stripeRefundMetadataKey tags every refund OpenRails creates with the intent
// ledger idempotency key, so the verifier can re-find the refund via READS
// (GET /v1/refunds) when the create's outcome was ambiguous (#358 phase B).
const stripeRefundMetadataKey = "openrails_idempotency_key"

// StripeAPICallError is a non-2xx response from the Stripe API: the request
// definitively reached Stripe and was answered. StatusCode lets money-mover
// intent handlers classify the failure (4xx clean refusal vs 429 throttling
// vs 5xx ambiguity) instead of guessing from message text.
type StripeAPICallError struct {
	StatusCode int
	Message    string
}

func (e *StripeAPICallError) Error() string { return e.Message }

type StripeRefundService struct {
	Config *config.Config
	Rails  config.RailMerchantAccountSet

	// BaseURL overrides the Stripe API root. Empty means the production
	// Stripe API (https://api.stripe.com). Tests set this to an httptest
	// server.
	BaseURL string
}

func (s *StripeRefundService) baseURL() string {
	if s != nil && strings.TrimSpace(s.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	}
	return "https://api.stripe.com"
}

type RefundParams struct {
	ChargeID string
	// Amount is Stripe minor units (typed Cents, #671); 0 = full refund.
	Amount         moneyutil.Cents
	Reason         string
	IdempotencyKey string
}

type RefundResult struct {
	ID            string            `json:"id"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	ChargeID      string            `json:"charge"`
	Status        string            `json:"status"`
	Reason        string            `json:"reason"`
	FailureReason string            `json:"failure_reason"`
	Metadata      map[string]string `json:"metadata"`
}

func (s *StripeRefundService) CreateRefund(ctx context.Context, params RefundParams) (*RefundResult, error) {
	if s == nil {
		return nil, fmt.Errorf("stripe refund service is not initialized")
	}
	_, secretKey, err := RequireStripeSecretKey(s.Rails)
	if err != nil {
		return nil, err
	}

	chargeID := strings.TrimSpace(params.ChargeID)
	if chargeID == "" {
		return nil, errors.New("charge_id or payment_intent_id is required")
	}

	values := url.Values{}
	if strings.HasPrefix(chargeID, "pi_") {
		values.Set("payment_intent", chargeID)
	} else {
		values.Set("charge", chargeID)
	}
	if params.Amount > 0 {
		values.Set("amount", strconv.FormatInt(int64(params.Amount), 10))
	}

	reason := strings.TrimSpace(params.Reason)
	if reason != "" {
		switch reason {
		case "duplicate", "fraudulent", "requested_by_customer":
			values.Set("reason", reason)
		default:
			values.Set("reason", "requested_by_customer")
		}
	}

	idempotencyKey := strings.TrimSpace(params.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = StripeRefundIdempotencyKey(chargeID, int64(params.Amount), reason)
	}
	// Metadata mirror of the idempotency key: the Idempotency-Key header
	// dedupes the WRITE, the metadata makes the refund re-findable via reads.
	values.Set("metadata["+stripeRefundMetadataKey+"]", idempotencyKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL()+"/v1/refunds", strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	stripeapi.SetIdempotencyKey(req, idempotencyKey)

	client := stripeapi.Client(s.Config, 30*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe refund request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read stripe refund response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe refund failed (%d)", resp.StatusCode)
		}
		return nil, &StripeAPICallError{StatusCode: resp.StatusCode, Message: msg}
	}

	var result RefundResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse stripe refund response: %w", err)
	}

	return &result, nil
}

// StripeRefundIdempotencyKey content-addresses one logical refund: the same
// (charge, amount, reason) is one provider mutation, however many times it is
// asked for. This is both the Stripe Idempotency-Key default and the intent
// ledger idempotency_key for stripe_refund intents.
func StripeRefundIdempotencyKey(chargeID string, amount int64, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "unspecified"
	}
	return fmt.Sprintf("openrails-refund:%s:%d:%s", strings.TrimSpace(chargeID), amount, reason)
}

func ResolveStripeRefundTarget(payment *models.Payment) (string, error) {
	if chargeID := strings.TrimSpace(metadataStringValue(payment.Metadata, "stripe_charge_id")); strings.HasPrefix(chargeID, "ch_") {
		return chargeID, nil
	}
	if paymentIntentID := strings.TrimSpace(metadataStringValue(payment.Metadata, "stripe_payment_intent_id")); strings.HasPrefix(paymentIntentID, "pi_") {
		return paymentIntentID, nil
	}

	transactionID := strings.TrimSpace(payment.TransactionID)
	if strings.HasPrefix(transactionID, "ch_") || strings.HasPrefix(transactionID, "pi_") {
		return transactionID, nil
	}

	return "", fmt.Errorf("%w: payment %s requires Stripe charge/payment_intent id (ch_... or pi_...) in metadata or transaction_id", ErrStripeRefundTargetMissing, payment.ID)
}

func metadataStringValue(metadata map[string]any, key string) string {
	v, ok := metadata[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func (s *StripeRefundService) GetRefund(ctx context.Context, refundID string) (*RefundResult, error) {
	if s == nil {
		return nil, fmt.Errorf("stripe refund service is not initialized")
	}
	_, secretKey, err := RequireStripeSecretKey(s.Rails)
	if err != nil {
		return nil, err
	}

	refundID = strings.TrimSpace(refundID)
	if refundID == "" {
		return nil, errors.New("refund_id is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL()+"/v1/refunds/"+refundID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)

	client := stripeapi.Client(s.Config, 0)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe refund fetch failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read stripe refund fetch response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe refund fetch failed (%d)", resp.StatusCode)
		}
		return nil, &StripeAPICallError{StatusCode: resp.StatusCode, Message: msg}
	}

	var result RefundResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse stripe refund response: %w", err)
	}

	return &result, nil
}

// FindRefundByIdempotencyKey re-discovers a refund via READS: lists the
// charge's (or payment intent's) refunds and matches the metadata mirror of
// the idempotency key. found=false with a nil error means Stripe answered and
// no such refund exists — the verifier's "verified NOT executed".
func (s *StripeRefundService) FindRefundByIdempotencyKey(ctx context.Context, chargeID, idempotencyKey string) (*RefundResult, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("stripe refund service is not initialized")
	}
	_, secretKey, err := RequireStripeSecretKey(s.Rails)
	if err != nil {
		return nil, false, err
	}
	chargeID = strings.TrimSpace(chargeID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if chargeID == "" || idempotencyKey == "" {
		return nil, false, errors.New("charge_id and idempotency key are required")
	}

	values := url.Values{"limit": {"100"}}
	if strings.HasPrefix(chargeID, "pi_") {
		values.Set("payment_intent", chargeID)
	} else {
		values.Set("charge", chargeID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL()+"/v1/refunds?"+values.Encode(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)

	client := stripeapi.Client(s.Config, 0)
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("stripe refund list failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read stripe refund list response: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := ParseStripeAPIError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe refund list failed (%d)", resp.StatusCode)
		}
		return nil, false, &StripeAPICallError{StatusCode: resp.StatusCode, Message: msg}
	}

	var list struct {
		Data []RefundResult `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, false, fmt.Errorf("failed to parse stripe refund list response: %w", err)
	}
	for i := range list.Data {
		if list.Data[i].Metadata[stripeRefundMetadataKey] == idempotencyKey {
			return &list.Data[i], true, nil
		}
	}
	return nil, false, nil
}
