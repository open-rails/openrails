package webhookutil

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/sigverify"
)

var (
	ErrWebhookEventIDMissing      = errors.New("missing webhook event id")
	ErrWebhookEventTypeMissing    = errors.New("missing webhook event type")
	ErrWebhookPayloadInvalid      = errors.New("invalid webhook payload")
	ErrWebhookEventTypeMismatch   = errors.New("webhook event type mismatch")
	ErrWebhookSignatureInvalid    = errors.New("invalid webhook signature")
	ErrWebhookSignatureMissing    = errors.New("missing webhook signature")
	ErrWebhookSignatureRequired   = errors.New("webhook signature required")
	ErrNMIWebhookSecretMissing    = errors.New("nmi webhook secret not configured")
	ErrNMIWebhookSignatureMissing = errors.New("missing webhook signature")
	ErrNMIWebhookSignatureInvalid = errors.New("invalid webhook signature")
)

type Prepared struct {
	Rail              string
	EventID           string
	EventType         string
	Body              []byte
	Signature         string
	SignatureVerified bool
}

// ErrWebhookRailRetired reports a webhook URL segment that is no longer a rail.
var ErrWebhookRailRetired = errors.New("retired webhook rail segment")

// retiredRailSegments maps a URL segment that used to fold onto a rail to the
// canonical segment. or#893: the webhook path's segment is the gateway KIND —
// a PSP key is not a rail, and folding one onto the other made two URLs mean
// the same endpoint while implying the PSP was routable by name.
var retiredRailSegments = map[string]string{
	"mobius": "nmi",
	// #795 accepted both spellings of the Basis Theory endpoint; the URL is
	// /webhooks/basistheory.
	"basis_theory": "basistheory",
}

// CanonicalRail normalizes a webhook URL's rail segment to the rail (or event
// source) it names. The segment is the gateway KIND — nmi, ccbill, stripe,
// solana, basistheory. Which PSP on that rail sent the event comes from the
// :account_id segment or the payload's own account identity, never from this
// segment: mobius and paykings both post to /webhooks/nmi.
func CanonicalRail(rail string) (string, error) {
	rail = strings.Trim(strings.ToLower(rail), " /")
	if canonical, retired := retiredRailSegments[rail]; retired {
		return "", fmt.Errorf("%w: /webhooks/%s was removed (or#893) — post to /webhooks/%s; a PSP is named by the account_id path segment or the payload's account identity, not by the rail segment", ErrWebhookRailRetired, rail, canonical)
	}
	// #795: the event source of a Basis Theory event is the CUSTODIAN itself
	// (or#879) — Basis Theory holds the card, NMI charges it, and only the
	// custodian emits these events.
	if rail == "basistheory" {
		return string(models.EventSourceBasisTheory), nil
	}
	return rail, nil
}

func PrepareCCBill(body []byte, eventType string) (Prepared, error) {
	normalizedBody, err := NormalizeCCBillPayload(body)
	if err != nil {
		return Prepared{}, fmt.Errorf("%w: %v", ErrWebhookPayloadInvalid, err)
	}

	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return Prepared{}, ErrWebhookEventTypeMissing
	}
	if bodyEventType, ok, err := ccbillBodyEventType(normalizedBody); err != nil {
		return Prepared{}, fmt.Errorf("%w: %v", ErrWebhookPayloadInvalid, err)
	} else if ok && !strings.EqualFold(strings.TrimSpace(bodyEventType), eventType) {
		return Prepared{}, fmt.Errorf("%w: query eventType %q does not match body eventType %q", ErrWebhookEventTypeMismatch, eventType, bodyEventType)
	}

	return Prepared{
		Rail:      subscriptions.RailCCBill,
		EventType: eventType,
		Body:      normalizedBody,
	}, nil
}

func ccbillBodyEventType(body []byte) (string, bool, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "", false, nil
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false, err
	}
	value, ok := payload["eventType"]
	if !ok || value == nil {
		return "", false, nil
	}
	return strings.TrimSpace(fmt.Sprint(value)), true, nil
}

func PrepareStripe(body []byte, secret, header string, tolerance time.Duration) (Prepared, error) {
	if strings.TrimSpace(secret) == "" {
		return Prepared{}, ErrWebhookSignatureRequired
	}

	header = strings.TrimSpace(header)
	if header == "" {
		return Prepared{}, ErrWebhookSignatureMissing
	}

	if err := VerifyStripeSignature(secret, header, body, tolerance); err != nil {
		return Prepared{}, fmt.Errorf("%w: %v", ErrWebhookSignatureInvalid, err)
	}

	eventID, eventType, err := ParseStripeEventMeta(body)
	if err != nil {
		return Prepared{}, fmt.Errorf("%w: %v", ErrWebhookPayloadInvalid, err)
	}

	return Prepared{
		Rail:              subscriptions.RailStripe,
		EventID:           eventID,
		EventType:         eventType,
		Body:              body,
		Signature:         header,
		SignatureVerified: true,
	}, nil
}

func PrepareNMI(rail string, body []byte, secret, header string) (Prepared, error) {
	signature, err := ValidateNMISignature(secret, body, header)
	if err != nil {
		return Prepared{}, err
	}

	var payload struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Prepared{}, fmt.Errorf("%w: %v", ErrWebhookPayloadInvalid, err)
	}

	payload.EventID = strings.TrimSpace(payload.EventID)
	if payload.EventID == "" {
		return Prepared{}, ErrWebhookEventIDMissing
	}

	return Prepared{
		// The caller canonicalizes the rail segment before it gets here.
		Rail:              strings.ToLower(strings.TrimSpace(rail)),
		EventID:           payload.EventID,
		EventType:         strings.TrimSpace(payload.EventType),
		Body:              body,
		Signature:         signature,
		SignatureVerified: true,
	}, nil
}

func ParseStripeEventMeta(body []byte) (string, string, error) {
	var payload struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(payload.ID) == "" || strings.TrimSpace(payload.Type) == "" {
		return "", "", fmt.Errorf("missing event id or type")
	}
	return payload.ID, payload.Type, nil
}

func ComputeUniqueKey(rail, eventID, eventType string, body []byte) string {
	rail = strings.ToLower(strings.TrimSpace(rail))
	eventID = strings.TrimSpace(eventID)
	if eventID != "" {
		return fmt.Sprintf("webhook:%s:%s", rail, eventID)
	}
	hash := sha256.Sum256(append([]byte(rail+"|"+eventType+"|"), body...))
	return fmt.Sprintf("webhook:%s:%s", rail, hex.EncodeToString(hash[:8]))
}

func NormalizeCCBillPayload(body []byte) ([]byte, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return body, nil
	}
	if body[0] == '{' || body[0] == '[' {
		return body, nil
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	payload := make(map[string]string, len(values))
	for key, val := range values {
		if len(val) > 0 {
			payload[key] = val[0]
		}
	}
	return json.Marshal(payload)
}

func VerifyStripeSignature(secret, header string, body []byte, tolerance time.Duration) error {
	return sigverify.VerifyStripe(secret, header, body, tolerance)
}

// NMISignatureTolerance is the replay window the public ingestion path enforces
// on NMI webhook timestamps.
const NMISignatureTolerance = 5 * time.Minute

// VerifyNMISignature authenticates an NMI webhook and rejects timestamps outside
// NMISignatureTolerance (replay protection for the public ingestion endpoint).
func VerifyNMISignature(secret, header string, body []byte) error {
	return VerifyNMISignatureWithTolerance(secret, header, body, NMISignatureTolerance)
}

// VerifyNMISignatureWithTolerance authenticates an NMI webhook's HMAC and, when
// tolerance > 0, rejects timestamps outside that window. A non-positive
// tolerance verifies the HMAC only and SKIPS the replay-window check — used by
// the queued re-verification path, where a job may legitimately be processed
// (or retried) long after the original delivery, so the window no longer
// applies but signature integrity must still hold. This is the single source of
// truth for NMI signature verification; do not reimplement it elsewhere.
func VerifyNMISignatureWithTolerance(secret, header string, body []byte, tolerance time.Duration) error {
	return sigverify.VerifyNMI(secret, header, body, tolerance)
}

func ValidateNMISignature(secret string, body []byte, phpHeader string) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", ErrNMIWebhookSecretMissing
	}

	phpHeader = strings.TrimSpace(phpHeader)
	if phpHeader != "" {
		if err := VerifyNMISignature(secret, phpHeader, body); err != nil {
			return "", fmt.Errorf("%w: %v", ErrNMIWebhookSignatureInvalid, err)
		}
		return phpHeader, nil
	}

	return "", ErrNMIWebhookSignatureMissing
}
