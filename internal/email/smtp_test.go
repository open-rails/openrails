package email

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
)

// The deployment's mail is from its own address; a merchant's from its
// profile address, the deployment's name filling an empty one. Both reach the
// server authenticated.
func TestSMTPSend(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: "SG.key"})
	s, err := NewSMTP(config.SMTPConfig{Host: srv.Host, Port: srv.Port, Username: "apikey", Password: "SG.key",
		From: config.EmailAddress{Name: "Deployment", Address: " noreply@deploy.test "}})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, s.CheckHealth(ctx))

	require.NoError(t, s.Send(ctx, config.Email{To: "ana@example.test", Subject: "Welcome", Text: "hi", HTML: "<p>hi</p>"}))
	require.NoError(t, s.Send(ctx, config.Email{From: config.EmailAddress{Address: "billing@shop.test"}, To: "bo@example.test", Subject: "Receipt", Text: "23 USD"}))
	got := srv.Wait(t, 2, 5*time.Second)
	require.Equal(t, "noreply@deploy.test", got[0].From)
	require.Equal(t, "\"Deployment\" <noreply@deploy.test>", got[0].Header.Get("From"))
	require.Equal(t, []string{"ana@example.test"}, got[0].To)
	require.Equal(t, "hi", got[0].Text)
	require.Equal(t, "<p>hi</p>", got[0].HTML)
	require.Equal(t, "apikey", got[0].Username)
	require.Equal(t, "billing@shop.test", got[1].From)
	require.Equal(t, "\"Deployment\" <billing@shop.test>", got[1].Header.Get("From"))

	none, err := NewSMTP(config.SMTPConfig{Host: srv.Host, Port: srv.Port, Username: "apikey", Password: "SG.key"})
	require.NoError(t, err)
	require.ErrorContains(t, none.Send(ctx, config.Email{To: "ana@example.test", Subject: "s", Text: "t"}), "no from address")
	require.Len(t, srv.Messages(), 2)

	_, err = NewSMTP(config.SMTPConfig{})
	require.ErrorContains(t, err, "host is required")
	_, err = NewSMTP(config.SMTPConfig{Host: "smtp.example", From: config.EmailAddress{Address: "not a mailbox"}})
	require.ErrorContains(t, err, "from")
}

// A server that refuses this client fails the health probe and the send.
func TestSMTPRefusal(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{})
	s, err := NewSMTP(config.SMTPConfig{Host: srv.Host, Port: srv.Port, From: config.EmailAddress{Address: "noreply@deploy.test"}})
	require.NoError(t, err)
	srv.Outage("421 4.3.2 service unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.ErrorContains(t, s.CheckHealth(ctx), "421")
	require.ErrorContains(t, s.Send(ctx, config.Email{To: "ana@example.test", Subject: "s", Text: "t"}), "421")
}
