package merchants

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestPaymentProviderDefinitions(t *testing.T) {
	expected := []PaymentProviderDefinition{
		{Rail: "nmi", DisplayName: "Credit Card", CredentialKeys: []string{"security_key", "webhook_signing_secret"}},
		{Rail: "ccbill", DisplayName: "Credit Card", CredentialKeys: []string{"salt", "datalink_username", "datalink_password"}},
		{Rail: "stripe", DisplayName: "Stripe", CredentialKeys: []string{"secret_key", "webhook_signing_secret", "webhook_signing_secret_thin"}},
		{Rail: "solana", DisplayName: "Solana", CredentialKeys: []string{}},
		{Rail: "vaulted_card", DisplayName: "Credit Card", CredentialKeys: []string{"api_key"}},
	}

	if got := PaymentProviderDefinitions(); !reflect.DeepEqual(got, expected) {
		t.Fatalf("PaymentProviderDefinitions() = %#v, want %#v", got, expected)
	}
}

func TestPaymentProviderConfigFromRowRedactsCredentials(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	secretName, err := PSPSecretName("stripe", "live", "acct_123", "secret_key")
	if err != nil {
		t.Fatal(err)
	}

	got := paymentProviderConfigFromRow(gen.OpenrailsPsp{
		ID:             uuid.New(),
		MerchantID:     uuid.New(),
		Rail:           "stripe",
		Environment:    "live",
		AccountID:      "acct_123",
		Archived:       false,
		Evidence:       []byte(`{"public_config":{"publishable_key":"pk_live_123"}}`),
		FirstSeenAt:    now,
		LastVerifiedAt: &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, []MerchantSecretStatus{{
		SecretDefinition: SecretDefinition{Name: secretName},
		Configured:       true,
	}})

	if got.PublicConfig["publishable_key"] != "pk_live_123" {
		t.Fatalf("public_config = %#v", got.PublicConfig)
	}
	if !got.Credentials["secret_key"].Configured {
		t.Fatal("secret_key should be marked configured")
	}
	if got.Credentials["secret_key"].LastValidatedAt == nil {
		t.Fatal("secret_key should carry last validation metadata")
	}
	if _, leaked := any(got.Credentials["secret_key"]).(string); leaked {
		t.Fatal("credential status leaked plaintext")
	}
}

func TestRailMerchantAccountSecretNameRejectsMerchantWritableSolanaPrivateKey(t *testing.T) {
	name, err := PSPSecretName("solana", "live", "authority", "private_key")
	if err != nil {
		t.Fatal(err)
	}
	if SecretWritable(name) {
		t.Fatal("solana private_key must stay platform-owned, not merchant-writable")
	}
}

func TestValidateScopedStripeCredentialDoesNotPersist(t *testing.T) {
	svc, err := NewSecretManagementService(NewMemorySecretStore())
	if err != nil {
		t.Fatal(err)
	}
	name, err := PSPSecretName("stripe", "live", "acct_123", "secret_key")
	if err != nil {
		t.Fatal(err)
	}
	id := merchant.ID(uuid.New())

	if err := svc.ValidateCredential(context.Background(), id, name, "not-stripe", nil); err == nil {
		t.Fatal("expected invalid scoped stripe secret to fail validation")
	}
	if statuses, err := svc.ListSecretStatuses(context.Background(), id); err != nil {
		t.Fatal(err)
	} else if len(statuses) != len(merchantSecretRegistry) {
		t.Fatalf("validation should not create provider-account secrets, got %d statuses", len(statuses))
	}
}
