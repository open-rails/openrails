package billing

import (
	"time"

	"github.com/google/uuid"
)

// PaymentProviderCredentialStatus describes a credential without returning its value.
type PaymentProviderCredentialStatus struct {
	Configured      bool       `json:"configured"`
	LastValidatedAt *time.Time `json:"last_validated_at,omitempty"`
	RotationVersion int        `json:"rotation_version,omitempty"`
}

type PaymentProviderConfig struct {
	ID              uuid.UUID                                  `json:"id"`
	Rail            string                                     `json:"rail"`
	Environment     string                                     `json:"environment"`
	AccountID       string                                     `json:"account_id"`
	Archived        bool                                       `json:"archived"`
	Drained         bool                                       `json:"drained"`
	OpenObligations int64                                      `json:"open_obligations"`
	PublicConfig    map[string]string                          `json:"public_config,omitempty"`
	Credentials     map[string]PaymentProviderCredentialStatus `json:"credentials"`
	FirstSeenAt     time.Time                                  `json:"first_seen_at"`
	LastVerifiedAt  *time.Time                                 `json:"last_validated_at,omitempty"`
	ReplacedAt      *time.Time                                 `json:"replaced_at,omitempty"`
	CreatedAt       time.Time                                  `json:"created_at"`
	UpdatedAt       time.Time                                  `json:"updated_at"`
	Revision        int64                                      `json:"revision"`
}

type PaymentProviderDefinition struct {
	Rail           string   `json:"rail"`
	DisplayName    string   `json:"display_name"`
	CredentialKeys []string `json:"credential_keys"`
}

type PaymentProviderListParams struct {
	Provider    string
	Environment string
	Status      string
}

type PaymentProviderList struct {
	Data                []PaymentProviderConfig     `json:"data"`
	ProviderDefinitions []PaymentProviderDefinition `json:"provider_definitions"`
}

// UpsertPaymentProviderParams changes one selected merchant's provider account.
// Credentials are write-only. Retry with the same operation identity and payload;
// the server owns credential storage paths, environment and publication authority.
type UpsertPaymentProviderParams struct {
	OperationID      uuid.UUID         `json:"operation_id"`
	ExpectedRevision *int64            `json:"expected_revision"`
	Enabled          *bool             `json:"enabled"`
	AccountID        string            `json:"account_id"`
	PublicConfig     map[string]string `json:"public_config"`
	Credentials      map[string]string `json:"credentials"`
	// RetireWebhookOverlap refuses the rotated-out webhook signing secret at
	// once instead of at its overlap expiry (SEC-29). With a new
	// webhook_signing_secret it rotates without any overlap.
	RetireWebhookOverlap bool   `json:"retire_webhook_overlap,omitempty"`
	LegacyEnvironment    string `json:"environment,omitempty"`
}

type ArchivePaymentProviderAccountParams struct {
	AllowLast bool `json:"allow_last"`
}
