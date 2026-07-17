package checkout

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestStripeCheckoutIdempotencyKey(t *testing.T) {
	t.Parallel()

	first := stripeCheckoutIdempotencyKey(" customer:key ")
	if first == "" {
		t.Fatal("idempotency key is empty")
	}
	if first != stripeCheckoutIdempotencyKey("customer:key") {
		t.Fatal("surrounding whitespace changed idempotency key")
	}
	if first == stripeCheckoutIdempotencyKey("customer:other-key") {
		t.Fatal("different checkout keys produced the same provider key")
	}
	if len(first) > 255 {
		t.Fatalf("provider key length = %d, exceeds Stripe limit", len(first))
	}
	if got := stripeCheckoutIdempotencyKey(strings.Repeat("x", 1<<10)); len(got) != len(first) {
		t.Fatalf("long input key length = %d, want %d", len(got), len(first))
	}
	if got := stripeCheckoutIdempotencyKey("   "); got != "" {
		t.Fatalf("blank key = %q, want empty", got)
	}
}

func TestIdempotentCheckoutSessionID(t *testing.T) {
	t.Parallel()

	merchantID := uuid.New()
	first := idempotentCheckoutSessionID(merchantID, " customer:key ")
	if first != idempotentCheckoutSessionID(merchantID, "customer:key") {
		t.Fatal("surrounding whitespace changed checkout session id")
	}
	if first == idempotentCheckoutSessionID(merchantID, "customer:other-key") {
		t.Fatal("different idempotency keys produced the same checkout session id")
	}
	if first == idempotentCheckoutSessionID(uuid.New(), "customer:key") {
		t.Fatal("checkout session id is not merchant scoped")
	}
}

func TestRejectCheckoutSessionPAN(t *testing.T) {
	t.Parallel()

	err := rejectCheckoutSessionPAN(&CheckoutSessionCreateRequest{
		Payment:  CheckoutSessionPaymentRequest{Rail: "stripe"},
		Metadata: map[string]string{"note": "4111 1111 1111 1111"},
	})
	if !errors.Is(err, ErrCheckoutSessionValidation) {
		t.Fatalf("reject checkout pan error = %v, want checkout validation error", err)
	}

	err = rejectCheckoutSessionPAN(&CheckoutSessionCreateRequest{
		Mode:    string(models.CheckoutSessionModeSolanaCancel),
		Payment: CheckoutSessionPaymentRequest{Rail: "solana", Wallet: "4111-1111-1111-1111"},
	})
	if !errors.Is(err, ErrCheckoutSessionValidation) {
		t.Fatalf("reject lifecycle pan error = %v, want checkout validation error", err)
	}

	err = rejectCheckoutSessionPAN(&CheckoutSessionCreateRequest{
		Payment:  CheckoutSessionPaymentRequest{Rail: "nmi", PaymentToken: "safe-token"},
		Metadata: map[string]string{"4111111111111111": "note"},
	})
	if !errors.Is(err, ErrCheckoutSessionValidation) {
		t.Fatalf("reject metadata key pan error = %v, want checkout validation error", err)
	}
}

func TestEqualOptionalUUID(t *testing.T) {
	t.Parallel()

	first := uuid.New()
	same := first
	different := uuid.New()
	if !equalOptionalUUID(nil, nil) || !equalOptionalUUID(&first, &same) {
		t.Fatal("equal optional UUIDs did not match")
	}
	if equalOptionalUUID(&first, nil) || equalOptionalUUID(nil, &first) || equalOptionalUUID(&first, &different) {
		t.Fatal("different optional UUIDs matched")
	}
}
