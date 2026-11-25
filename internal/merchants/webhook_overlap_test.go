package merchants

import (
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/merchantdocs"
)

func TestCCBillWebhookTokenRotationKeepsThePreviousForTheOverlap(t *testing.T) {
	const current, next = "ccbill-current-webhook-token-0123456789", "ccbill-next-webhook-token-012345678901"
	clock := clockwork.NewFakeClockAt(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC))
	s := (&Service{}).WithClock(clock)
	p := merchantdocs.PSP{Rail: "ccbill", Secrets: map[string]string{"salt": "s", "webhook_token": current}}
	rotated, err := s.applyCredentials(&p, map[string]string{"webhook_token": next}, false)
	if err != nil || !rotated {
		t.Fatalf("rotated=%v err=%v", rotated, err)
	}
	if p.Secrets["webhook_token"] != next || p.Secrets["webhook_token_previous"] != current {
		t.Fatalf("secrets after rotation: current %q previous %q", p.Secrets["webhook_token"], p.Secrets["webhook_token_previous"])
	}
	if got := overlapUntil(p.Settings); !got.Equal(clock.Now().Add(webhookSecretOverlap)) {
		t.Fatalf("overlap ends %s", got)
	}
	if _, err := s.applyCredentials(&p, map[string]string{"webhook_token_previous": current}, false); err == nil {
		t.Fatal("a supplied webhook_token_previous is refused")
	}
	if _, err := s.applyCredentials(&p, nil, true); err != nil || p.Secrets["webhook_token_previous"] != "" || p.Settings[WebhookOverlapExpiresKey] != nil {
		t.Fatalf("retire keeps %q, %v (err %v)", p.Secrets["webhook_token_previous"], p.Settings[WebhookOverlapExpiresKey], err)
	}
}

func TestCCBillWebhookTokenShape(t *testing.T) {
	for token, ok := range map[string]bool{
		"ccbill-current-webhook-token-0123456789": true,
		"0123456789abcdefghijABCDEFGHIJ_-":        true,
		"too-short-0123456789abcdefghijk":         false,
		"has/a/slash-0123456789abcdefghijklmn":    false,
		"has?query=0123456789abcdefghijklmnopq":   false,
	} {
		if err := validateCredentialValue("ccbill", "webhook_token", token); (err == nil) != ok {
			t.Errorf("%q: err %v, want ok=%v", token, err, ok)
		}
	}
}
