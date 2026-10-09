//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest"
)

type emailSweep struct{}

func (emailSweep) Kind() string { return "openrails.notification_email_sweep" }

// mailTo is the first message mail delivered to address, after a sweep
// delivers what is pending.
func (w *world) mailTo(mail *mailbox, address string) openrails.Email {
	t := w.t
	t.Helper()
	res, err := w.jobs.Insert(t.Context(), emailSweep{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(res.Job.ID)
	var sent openrails.Email
	require.Eventually(t, func() bool {
		for _, e := range mail.all() {
			if e.To == address {
				sent = e
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "an email to %s; sent %+v", address, mail.all())
	return sent
}

// Embedded beside the host's directory, OpenRails keeps no copy: a receipt
// goes to the email the directory holds now, and the admin read and search
// ask it. The directory is the one source: provisioning cannot be mounted
// beside it.
func TestContactsComeFromTheHostDirectory(t *testing.T) {
	t.Parallel()
	directory, mail := &openrailstest.Contacts{}, &mailbox{}
	w := prepareWorld(t, 12)
	w.deps = func(d *openrails.Deps) { d.Contacts, d.Email = directory, mail }
	w.start()
	ctx := t.Context()

	c := w.newCustomer()
	directory.Put(openrails.Contact{ID: c.id, Email: "member@host.test", Name: "Member One", Username: "member_one"})
	price := w.membership("content:members", 9_990_000)
	c.subscribe(embedded, "stripe", price.ID.String(), "content:members", c.saveCard("stripe", visa))
	receipt := w.mailTo(mail, "member@host.test")
	require.NotEmpty(t, receipt.Subject)

	read, err := w.client[remote].ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{c.cid()}})
	require.NoError(t, err)
	contact := read.Items[0].Contact
	require.NotNil(t, contact)
	require.Equal(t, "member@host.test", *contact.Email)
	require.Equal(t, "Member One", *contact.Name)
	require.Nil(t, contact.SyncedAt, "a live read keeps no copy")
	found, err := w.client[remote].ListCustomers(ctx, billing.CustomerListParams{Search: "MEMBER_ONE"})
	require.NoError(t, err)
	require.Len(t, found.Items, 1)
	require.Equal(t, c.cid(), found.Items[0].ID)

	// The directory changes; the next read is current.
	directory.Put(openrails.Contact{ID: c.id, Email: "renamed@host.test", Name: "Member One", Username: "member_one"})
	profile, err := w.client[remote].GetCustomer(ctx, c.cid())
	require.NoError(t, err)
	require.Equal(t, "renamed@host.test", *profile.Contact.Email)

	_, err = w.rt.Routes(openrails.Routes{Auth: w.auth, Prefix: "/other", Provisioning: true})
	require.ErrorContains(t, err, "Deps.Contacts", "one source of truth")
	_, err = w.rt.SCIMHandler()
	require.Error(t, err)
}

// Without the host's directory in process, the merchant's directory pushes
// its users over SCIM, and a receipt goes to the pushed email; a change it
// pushes later is where the next one goes.
func TestReceiptsGoToThePushedEmail(t *testing.T) {
	t.Parallel()
	mail := &mailbox{}
	w := prepareWorld(t, 12)
	w.deps = func(d *openrails.Deps) { d.Email = mail }
	w.mount = func(r *openrails.Routes) { r.Provisioning = true }
	w.start()
	ctx := t.Context()
	token, err := w.client[embedded].CreateProvisioningToken(ctx, billing.CreateProvisioningTokenParams{Name: "directory"})
	require.NoError(t, err)
	push := func(method, path string, body any, want int) {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, method, w.server.URL+mountPrefix+"/scim/v2"+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token.Token)
		req.Header.Set("Content-Type", "application/scim+json")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		require.Equal(t, want, res.StatusCode, "%s %s: %s", method, path, out)
	}
	user := func(email string) map[string]any {
		return map[string]any{
			"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "userName": "pushed",
			"name": map[string]any{"formatted": "Pushed Member"}, "emails": []any{map[string]any{"value": email, "primary": true}}, "active": true,
		}
	}

	c := w.newCustomer()
	created := user("pushed@directory.test")
	created["externalId"] = c.id
	push(http.MethodPost, "/Users", created, http.StatusCreated)
	price := w.membership("content:members", 9_990_000)
	c.subscribe(embedded, "stripe", price.ID.String(), "content:members", c.saveCard("stripe", visa))
	w.mailTo(mail, "pushed@directory.test")

	profile, err := w.client[remote].GetCustomer(ctx, c.cid())
	require.NoError(t, err)
	contact := profile.Contact
	require.NotNil(t, contact)
	require.Equal(t, "pushed@directory.test", *contact.Email)
	require.Equal(t, "pushed", *contact.Username)
	require.True(t, *contact.Active)
	require.NotNil(t, contact.SyncedAt, "the copy says when it was pushed")

	push(http.MethodPut, "/Users/"+c.id, user("moved@directory.test"), http.StatusOK)
	extra := w.membership("content:extra", 4_990_000)
	c.subscribeAgain(embedded, "stripe", extra.ID.String(), "content:extra", c.saveCard("stripe", mastercard))
	w.mailTo(mail, "moved@directory.test")
}
