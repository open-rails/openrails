package smtpmail

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/helpers/smtp"
	"github.com/open-rails/helpers/smtp/smtptest"

	"github.com/open-rails/openrails"
)

func TestEmailSenderFrom(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{})
	newSender := func(from string) openrails.EmailSender {
		s, err := smtp.New(smtp.Config{Host: srv.Host, Port: srv.Port, From: from})
		if err != nil {
			t.Fatal(err)
		}
		return EmailSender(s)
	}
	ctx := context.Background()

	if err := newSender("Shop <shop@example.test>").Send(ctx, openrails.Email{To: "a@example.test", Subject: "default", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := newSender("Shop <shop@example.test>").Send(ctx, openrails.Email{From: openrails.EmailAddress{Address: "m@example.test"}, To: "a@example.test", Subject: "merchant", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	got := srv.Wait(t, 2, 5*time.Second)
	if got[0].From != "shop@example.test" || got[1].From != "m@example.test" {
		t.Fatalf("envelope from = %q, %q", got[0].From, got[1].From)
	}
	if h := got[1].Header.Get("From"); h != `"Shop" <m@example.test>` {
		t.Fatalf("merchant From header = %q, want the default name with the merchant's address", h)
	}

	if err := newSender("").Send(ctx, openrails.Email{To: "a@example.test", Subject: "none", Text: "x"}); err == nil {
		t.Fatal("a message with no from address was sent")
	}
}
