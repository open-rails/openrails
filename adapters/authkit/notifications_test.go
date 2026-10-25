package authkit

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

func TestNotificationsVerifyIssuerBeforeMappingAccount(t *testing.T) {
	merchant, customer := billing.MerchantID(uuid.New()), billing.CustomerID(uuid.New())
	channel := Notifications(nil, merchant, "https://accounts.example")
	for _, tc := range []struct {
		name, issuer string
		deliver      bool
	}{
		{"native", "", true},
		{"explicit-issuer", "https://accounts.example", true},
		{"foreign-same-uuid", "https://untrusted.example", false},
		{"issuer-is-exact", "https://accounts.example/", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, deliver, err := channel.Recipient(context.Background(), openrails.NotificationRecipient{MerchantID: merchant, CustomerID: customer, Issuer: tc.issuer})
			if err != nil || deliver != tc.deliver {
				t.Fatalf("mapping returned account %q, deliver %v, error %v", id, deliver, err)
			}
			if deliver && id != customer.UUID().String() {
				t.Fatalf("mapped a different account: %s", id)
			}
			if !deliver && id != "" {
				t.Fatalf("foreign issuer exposed an account mapping: %s", id)
			}
		})
	}
	if _, _, err := channel.Recipient(context.Background(), openrails.NotificationRecipient{}); err == nil {
		t.Fatal("missing merchant/customer identity was accepted")
	}
	foreign := openrails.NotificationRecipient{MerchantID: billing.MerchantID(uuid.New()), CustomerID: customer, Issuer: "https://accounts.example"}
	if id, deliver, err := channel.Recipient(context.Background(), foreign); err != nil || deliver || id != "" {
		t.Fatalf("another merchant reached the fixed-scope publisher: account=%q deliver=%v err=%v", id, deliver, err)
	}
	unbound := Notifications(nil, billing.MerchantID{})
	if _, _, err := unbound.Recipient(context.Background(), openrails.NotificationRecipient{MerchantID: merchant, CustomerID: customer}); err == nil {
		t.Fatal("unbound channel accepted a recipient")
	}
}
