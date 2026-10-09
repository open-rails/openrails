//go:build e2e && integration

package subscriptions_test

import (
	"strings"
	"testing"
	"time"

	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest"
)

// A membership's receipt reaches the customer through the built-in sender
// (Config.SMTP): rendered by the engine and sent over SMTP with credentials,
// from the merchant's profile address, to the address the directory holds.
func TestMembershipReceiptArrivesBySMTP(t *testing.T) {
	t.Parallel()
	srv := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: "SG.e2e-key"})
	directory := &openrailstest.Contacts{}
	w := prepareWorld(t, 12, func(c *openrails.Config) {
		c.SMTP = &openrails.SMTPConfig{Host: srv.Host, Port: srv.Port, Username: "apikey", Password: "SG.e2e-key",
			From: openrails.EmailAddress{Name: "Merchant Billing", Address: "noreply@deploy.test"}}
	})
	w.deps = func(d *openrails.Deps) { d.Contacts = directory }
	w.start()
	ctx := t.Context()
	const from, to = "billing@merchant.test", "member@host.test"
	require.NoError(t, w.applySettings(ctx, billing.MerchantSettings{Profile: &billing.MerchantProfile{FromEmail: from}}))

	c := w.newCustomer()
	directory.Put(openrails.Contact{ID: c.id, Email: to, Name: "Member One", Username: "member_one"})
	price := w.membership("content:members", 9_990_000)
	c.subscribe(embedded, "nmi", price.ID.String(), "content:members", c.saveCard("nmi", visa))
	res, err := w.jobs.Insert(ctx, emailSweep{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(res.Job.ID)

	m := srv.Wait(t, 1, 20*time.Second)[0]
	require.Equal(t, []string{to}, m.To)
	require.Equal(t, from, m.From, "the merchant's address is the envelope sender")
	sender, err := m.Header.AddressList("From")
	require.NoError(t, err)
	require.Equal(t, from, sender[0].Address)
	require.Equal(t, "apikey", m.Username)
	require.NotEmpty(t, m.Subject)
	require.NotEmpty(t, strings.TrimSpace(m.Text))
	require.NotEmpty(t, strings.TrimSpace(m.HTML))
	require.Contains(t, m.Text+m.HTML, "9.99", "the receipt names what was paid")
}
