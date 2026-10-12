package merchantdocs

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
)

func TestConfigurationChangesCountSecurityTargets(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	id := billing.AlertWebhookID(uuid.New())
	old := Merchant{DisplayName: "Old", Settings: billing.MerchantSettings{Profile: &billing.MerchantProfile{LogoURL: "https://old.test/logo"}}, AlertWebhooks: []AlertWebhook{{ID: id, Name: "old", URL: "https://alerts.test/private-token", Enabled: true}}}
	next := old
	next.DisplayName = "New"
	next.Settings.Profile = &billing.MerchantProfile{LogoURL: "https://new.test/logo"}
	next.AlertWebhooks = append([]AlertWebhook(nil), old.AlertWebhooks...)
	next.AlertWebhooks[0].Name = "New name"
	next.AlertWebhooks[0].UpdatedAt = time.Now()
	changes, err := configurationChanges(key, MerchantDoc, old, next)
	if err != nil || len(changes) != 0 {
		t.Fatalf("cosmetic edit: %v %v", changes, err)
	}
	next.AlertWebhooks[0].Enabled = false
	next.AlertWebhooks = append(next.AlertWebhooks, AlertWebhook{ID: billing.AlertWebhookID(uuid.New()), URL: "https://other.test/secret", Enabled: true})
	next.Secrets.SCIMToken = "credential"
	changes, err = configurationChanges(key, MerchantDoc, old, next)
	if err != nil || len(changes) != 3 {
		t.Fatalf("credential + two destinations: %v %v", changes, err)
	}
	for _, change := range changes {
		if len(change.Before) != 64 || len(change.After) != 64 || strings.Contains(change.Target, "secret") {
			t.Fatalf("unsafe fingerprint: %+v", change)
		}
	}
}

func TestConfigurationPSPRotationAndReplay(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	old := PSP{Rail: "nmi", Environment: "test", AccountID: "account", Secrets: map[string]string{"security_key": "first"}}
	changes, err := configurationChanges(key, PSPDoc("cards"), old, old)
	if err != nil || len(changes) != 0 {
		t.Fatalf("no-op: %v %v", changes, err)
	}
	next := old
	next.Secrets = map[string]string{"security_key": "second"}
	changes, err = configurationChanges(key, PSPDoc("cards"), old, next)
	if err != nil || len(changes) != 1 {
		t.Fatalf("rotation: %v %v", changes, err)
	}
	if _, err = configurationChanges(nil, PSPDoc("cards"), old, next); err == nil {
		t.Fatal("missing fingerprint key accepted")
	}
}
