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
		// or#879/or#880: vaulted_card is NOT a rail — it is NMI with the card
		// held by a third-party custodian. The rail is gone (0031) and the
		// custodian's own key is NOT an NMI credential: it belongs to the
		// custodian account (0053), not to whichever gateway it proxies into.
		{Rail: "nmi", DisplayName: "Credit Card", CredentialKeys: []string{"security_key", "webhook_signing_secret"}},
		{Rail: "ccbill", DisplayName: "Credit Card", CredentialKeys: []string{"salt", "datalink_username", "datalink_password"}},
		{Rail: "stripe", DisplayName: "Stripe", CredentialKeys: []string{"secret_key", "webhook_signing_secret", "webhook_signing_secret_thin", "webhook_signing_secret_previous"}},
		{Rail: "solana", DisplayName: "Solana", CredentialKeys: []string{}},
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
		Evidence:       []byte(`{"public_config":{"publishable_key":"pk_live_123"},"credentials_validated":true}`),
		FirstSeenAt:    now,
		LastVerifiedAt: &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, []MerchantSecretStatus{{Name: secretName, Configured: true}})

	if got.PublicConfig["publishable_key"] != "pk_live_123" {
		t.Fatalf("public_config = %#v", got.PublicConfig)
	}
	if !got.Credentials["secret_key"].Configured {
		t.Fatal("secret_key should be marked configured")
	}
	if got.Credentials["secret_key"].LastValidatedAt == nil {
		t.Fatal("secret_key should carry last validation metadata")
	}
	if got.Credentials["webhook_signing_secret"].LastValidatedAt != nil {
		t.Fatal("format-only webhook secret must not carry live validation metadata")
	}
	if _, leaked := any(got.Credentials["secret_key"]).(string); leaked {
		t.Fatal("credential status leaked plaintext")
	}
}

func TestPaymentProviderConfigFromRowHidesUnprovenTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	secretName, err := PSPSecretName("nmi", "live", "gateway", "security_key")
	if err != nil {
		t.Fatal(err)
	}

	got := paymentProviderConfigFromRow(gen.OpenrailsPsp{
		Rail:           "nmi",
		Environment:    "live",
		AccountID:      "gateway",
		LastVerifiedAt: &now,
	}, []MerchantSecretStatus{{Name: secretName, Configured: true}})

	if got.LastVerifiedAt != nil || got.Credentials["security_key"].LastValidatedAt != nil {
		t.Fatal("an auto-stamped timestamp without probe evidence must stay hidden")
	}
}

func TestCredentialValidatedAtOnlyMarksLiveProbedCredentials(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		rail string
		key  string
		want bool
	}{
		{name: "stripe secret key", rail: "stripe", key: "secret_key", want: true},
		{name: "stripe webhook secret", rail: "stripe", key: "webhook_signing_secret"},
		{name: "nmi security key", rail: "nmi", key: "security_key", want: true},
		{name: "nmi webhook secret", rail: "nmi", key: "webhook_signing_secret"},
		{name: "ccbill datalink username", rail: "ccbill", key: "datalink_username", want: true},
		{name: "ccbill datalink password", rail: "ccbill", key: "datalink_password", want: true},
		{name: "ccbill salt", rail: "ccbill", key: "salt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := credentialValidatedAt(tt.rail, tt.key, &now)
			if (got != nil) != tt.want {
				t.Fatalf("credentialValidatedAt(%q, %q) validated = %t, want %t", tt.rail, tt.key, got != nil, tt.want)
			}
		})
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
	} else if len(statuses) != 0 {
		t.Fatalf("validation should not create provider-account secrets, got %d statuses", len(statuses))
	}
}
