//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

const hostDirectoryPath = "/directory/scim/v2"
const hostSCIMUserSchema = "urn:ietf:params:scim:schemas:core:2.0:User"

func TestCustomerDirectory(t *testing.T) {
	t.Run("OR-J07-S01/provision-patch-replace-delete-preserves-issuer-subject", func(t *testing.T) {
		w := newStandaloneWorld(t)
		key := w.Merchants["alpha"].DirectoryKey
		id := uuid.NewString()
		input := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": "directory-alice", "displayName": "Directory Alice", "emails": []any{map[string]any{"value": "alice@directory.invalid", "primary": true}}, "active": true}
		status, raw := w.request(t, "POST", hostDirectoryPath+"/Users", key, input)
		require.Equal(t, http.StatusCreated, status, string(raw))
		var created map[string]any
		require.NoError(t, json.Unmarshal(raw, &created))
		remoteID := created["id"].(string)
		require.NotEmpty(t, remoteID)
		require.Equal(t, id, created["externalId"])
		var token billing.CustomerToken
		w.command(t, map[string]any{"Op": "external-user-token", "Merchant": "alpha", "ID": id}, &token)
		require.NotEmpty(t, token.AccessToken)
		status, raw = w.request(t, "GET", "/v1/me", token.AccessToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		customer, err := w.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(id)))
		require.NoError(t, err)
		require.NotNil(t, customer.Contact)
		require.Equal(t, "Directory Alice", *customer.Contact.Name)
		require.Equal(t, "alice@directory.invalid", *customer.Contact.Email)
		patch := map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": []any{map[string]any{"op": "replace", "path": "displayName", "value": "Alice Renamed"}, map[string]any{"op": "replace", "path": "emails", "value": []any{map[string]any{"value": "renamed@directory.invalid", "primary": true}}}}}
		status, raw = w.request(t, "PATCH", hostDirectoryPath+"/Users/"+remoteID, key, patch)
		require.Equal(t, http.StatusOK, status, string(raw))
		customer, err = w.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(id)))
		require.NoError(t, err)
		require.Equal(t, id, customer.ID.String())
		require.Equal(t, "Alice Renamed", *customer.Contact.Name)
		require.Equal(t, "renamed@directory.invalid", *customer.Contact.Email)
		status, raw = w.request(t, "GET", hostDirectoryPath+"/Users?filter="+url.QueryEscape(`externalId eq "`+id+`"`), key, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), `"totalResults":1`)
		require.Contains(t, string(raw), remoteID)
		// A failed multi-operation patch is atomic; an earlier valid change cannot leak.
		bad := map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": []any{map[string]any{"op": "replace", "path": "displayName", "value": "Must Roll Back"}, map[string]any{"op": "replace", "path": "id", "value": "new-owner"}}}
		status, raw = w.request(t, "PATCH", hostDirectoryPath+"/Users/"+remoteID, key, bad)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		status, raw = w.request(t, "GET", hostDirectoryPath+"/Users/"+remoteID, key, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), "Alice Renamed")
		require.NotContains(t, string(raw), "Must Roll Back")
		// Another merchant, even with a provisioner, cannot read the source resource.
		status, raw = w.request(t, "GET", hostDirectoryPath+"/Users/"+remoteID, w.Merchants["beta"].DirectoryKey, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		input["displayName"] = "Replacement"
		delete(input, "emails")
		status, raw = w.request(t, "PUT", hostDirectoryPath+"/Users/"+remoteID, key, input)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NotContains(t, string(raw), "renamed@directory.invalid")
		status, raw = w.request(t, "DELETE", hostDirectoryPath+"/Users/"+remoteID, key, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		status, raw = w.request(t, "GET", hostDirectoryPath+"/Users/"+remoteID, key, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		customer, err = w.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(id)))
		require.NoError(t, err)
		require.Equal(t, id, customer.ID.String())
		require.Nil(t, customer.Contact)
	})
	t.Run("OR-J07-S02/replay-and-other-issuer-stay-distinct", func(t *testing.T) {
		w := newStandaloneWorld(t)
		alpha := w.Merchants["alpha"]
		var other iam.APIKeyCreated
		w.command(t, map[string]any{"Op": "new-directory", "Merchant": alpha.ID.String(), "ID": "https://second-issuer.critical.invalid"}, &other)
		id := uuid.NewString()
		input := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": "same-name", "displayName": "First Issuer", "emails": []any{map[string]any{"value": "same@directory.invalid"}}}
		status, raw := w.request(t, "POST", hostDirectoryPath+"/Users", alpha.DirectoryKey, input)
		require.Equal(t, 201, status, string(raw))
		var first map[string]any
		require.NoError(t, json.Unmarshal(raw, &first))
		status, raw = w.request(t, "POST", hostDirectoryPath+"/Users", alpha.DirectoryKey, input)
		require.Equal(t, 409, status, string(raw))
		require.Contains(t, string(raw), "uniqueness")
		input["displayName"] = "Second Issuer"
		status, raw = w.request(t, "POST", hostDirectoryPath+"/Users", other.Secret, input)
		require.Equal(t, 201, status, string(raw))
		var second map[string]any
		require.NoError(t, json.Unmarshal(raw, &second))
		require.NotEqual(t, first["id"], second["id"])
		status, raw = w.request(t, "GET", hostDirectoryPath+"/Users/"+first["id"].(string), other.Secret, nil)
		require.Equal(t, 404, status, string(raw))
		for _, row := range []struct{ key, name string }{{alpha.DirectoryKey, "First Issuer"}, {other.Secret, "Second Issuer"}} {
			status, raw = w.request(t, "GET", hostDirectoryPath+"/Users?filter="+url.QueryEscape(`externalId eq "`+id+`"`), row.key, nil)
			require.Equal(t, 200, status, string(raw))
			require.Contains(t, string(raw), `"totalResults":1`)
			require.Contains(t, string(raw), row.name)
		}
		// Bulk reports each item's result instead of turning a duplicate into success.
		bulk := map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:BulkRequest"}, "Operations": []any{map[string]any{"method": "POST", "path": "/Users", "bulkId": "duplicate", "data": input}, map[string]any{"method": "POST", "path": "/Users", "bulkId": "new", "data": map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": uuid.NewString(), "userName": "another"}}}}
		status, raw = w.request(t, "POST", hostDirectoryPath+"/Bulk", alpha.DirectoryKey, bulk)
		require.Equal(t, 200, status, string(raw))
		require.Contains(t, string(raw), `"status":"409"`)
		require.Contains(t, string(raw), `"status":"201"`)
	})
	t.Run("OR-J07-S03/revocation-and-wrong-authority-stop-directory-writes", func(t *testing.T) {
		w := newStandaloneWorld(t)
		alpha := w.Merchants["alpha"]
		id := uuid.NewString()
		input := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": "revocation-sentinel", "displayName": "Preserved"}
		status, raw := w.request(t, "POST", hostDirectoryPath+"/Users", alpha.DirectoryKey, input)
		require.Equal(t, 201, status, string(raw))
		var user map[string]any
		require.NoError(t, json.Unmarshal(raw, &user))
		for _, key := range []string{alpha.Key, w.AliceToken, ""} {
			status, raw = w.request(t, "PUT", hostDirectoryPath+"/Users/"+user["id"].(string), key, map[string]any{"externalId": id, "userName": "overwritten"})
			require.Contains(t, []int{401, 403}, status, string(raw))
		}
		var revoked bool
		w.command(t, map[string]any{"Op": "revoke-directory-key", "Merchant": alpha.ID.String(), "ID": alpha.DirectoryKeyID}, &revoked)
		require.True(t, revoked)
		status, raw = w.request(t, "DELETE", hostDirectoryPath+"/Users/"+user["id"].(string), alpha.DirectoryKey, nil)
		require.Equal(t, 401, status, string(raw))
		// A replacement issuer-bound key sees the same resource after denied mutations.
		var token billing.CustomerToken
		w.command(t, map[string]any{"Op": "external-user-token", "Merchant": "alpha", "ID": id}, &token)
		require.NotEmpty(t, token.AccessToken)
		status, raw = w.request(t, "GET", "/v1/me", token.AccessToken, nil)
		require.Equal(t, 200, status, string(raw))
		customer, err := w.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(id)))
		require.NoError(t, err)
		require.NotNil(t, customer.Contact)
		require.Equal(t, "Preserved", *customer.Contact.Name)
		// The embedded copied-directory profile has its distinct revocable provisioning token.
		a := newAuthorityWorld(t, func(w *world) { w.deps.UserInfo = nil })
		provisioner, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "owned directory"})
		require.NoError(t, err)
		status, raw = a.request(t, "POST", "/v1/app/scim/v2/Users", provisioner.Token, input)
		require.Equal(t, 201, status, string(raw))
		status, raw = a.request(t, "GET", "/v1/admin/customers", provisioner.Token, nil)
		require.Contains(t, []int{401, 403}, status, string(raw))
		require.NoError(t, a.alphaRemote.DeleteProvisioningToken(t.Context(), provisioner.ID))
		status, raw = a.request(t, "DELETE", "/v1/app/scim/v2/Users/"+id, provisioner.Token, nil)
		require.Equal(t, 401, status, string(raw))
		stored, err := a.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(id)))
		require.NoError(t, err)
		require.Equal(t, "Preserved", *stored.Contact.Name)
	})
	t.Run("OR-J07-S10/a-search-finds-the-merchants-customers-past-people-who-are-not", func(t *testing.T) {
		a := newAuthorityWorld(t)
		prefix := "lookalike" + uuid.NewString()[:8]
		user := func(name string) iam.User {
			t.Helper()
			u, err := a.auth.CreateUser(t.Context(), iam.NewUser{Email: name + "@example.com", Username: name, Password: authtest.Password, EmailVerified: true})
			require.NoError(t, err)
			return u
		}
		// The directory answers in creation order: a dozen people who are no
		// customer of alpha match first.
		for i := range 12 {
			user(fmt.Sprintf("%sp%02d", prefix, i))
		}
		buyer := billing.CustomerID(uuid.MustParse(user(prefix + "buyer").ID))
		_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: buyer, Currency: "USD", Amount: 1000, Source: "host", SourceID: "searched-customer"}})
		require.NoError(t, err)
		for _, client := range []*openrails.Client{a.alpha, a.alphaRemote} {
			found, err := client.ListCustomers(t.Context(), billing.CustomerListParams{Search: prefix, PageRequest: billing.PageRequest{Limit: 10}})
			require.NoError(t, err)
			require.Len(t, found.Items, 1, "the one customer among the directory's matches")
			require.Equal(t, buyer, found.Items[0].ID)
			require.NotNil(t, found.Items[0].Contact)
			require.Equal(t, prefix+"buyer", *found.Items[0].Contact.Username)
		}
	})
	t.Run("OR-J07-S11/a-kept-search-lists-the-exact-match-first-then-the-newest", func(t *testing.T) {
		a := newAuthorityWorld(t, func(w *world) { w.deps.UserInfo = nil })
		token, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
		named := map[billing.CustomerID]string{}
		for _, name := range []string{"joanne", "ann", "annabel", "hannah"} {
			id := uuid.NewString()
			status, raw := a.request(t, "POST", scimUsers, token.Token, scimUser(id, name))
			require.Equal(t, http.StatusCreated, status, string(raw))
			named[billing.CustomerID(uuid.MustParse(id))] = name
		}
		for limit, want := range map[int][]string{10: {"ann", "hannah", "annabel", "joanne"}, 2: {"ann", "hannah"}} {
			found, err := a.alpha.ListCustomers(t.Context(), billing.CustomerListParams{Search: "ann", PageRequest: billing.PageRequest{Limit: limit}})
			require.NoError(t, err)
			var got []string
			for _, c := range found.Items {
				got = append(got, named[c.ID])
			}
			require.Equal(t, want, got, "the username it is first, then the newest")
		}
	})
	t.Run("OR-J07-S12/a-customer-tokens-verified-email-is-its-kept-contact-and-its-receipts-address", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) {
			w.deps.UserInfo = nil
			w.deps.Email = mail
		})
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		status, raw := a.request(t, http.MethodGet, "/v1/me", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		customer, err := a.alpha.GetCustomer(t.Context(), alice)
		require.NoError(t, err)
		require.NotNil(t, customer.Contact, "the token's claims are the customer's contact")
		require.Equal(t, a.alice.Email, deref(customer.Contact.Email))
		require.Equal(t, a.alice.Username, deref(customer.Contact.Username))
		paidExternally(t, a, alice, messageSale(t, a), "claimed-receipt")
		startFleet(t, a.world)
		log := settledMessages(t, a.world, alice, 1)
		require.Equal(t, billing.MessageSent, log[0].Status)
		require.Equal(t, a.alice.Email, deref(log[0].Recipient))
		require.Len(t, mail.to(a.alice.Email), 1)
	})
	t.Run("OR-J07-S13/token-claims-never-undo-a-directory-push-or-an-erasure", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) {
			w.deps.UserInfo = nil
			w.deps.Email = mail
		})
		provisioner, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
		alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
		signedIn := func(token string) *billing.CustomerContact {
			t.Helper()
			status, raw := a.request(t, http.MethodGet, "/v1/me", token, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			customer, err := a.alpha.GetCustomer(t.Context(), alice)
			require.NoError(t, err)
			return customer.Contact
		}
		unverified := a.signedToken(t, func(_ map[string]any, claims map[string]any) {
			claims["email"], claims["email_verified"], claims["username"] = "unverified@claims.invalid", false, "alice-claimed"
		})
		contact := signedIn(unverified)
		require.NotNil(t, contact)
		require.Nil(t, contact.Email, "an unverified email is never kept")
		require.Equal(t, "alice-claimed", deref(contact.Username))
		contact = signedIn(a.aliceToken)
		require.Equal(t, a.alice.Email, deref(contact.Email), "undated claims replace undated claims")
		require.Equal(t, a.alice.Username, deref(contact.Username))
		user := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": a.alice.ID, "userName": "alice-directory", "displayName": "Alice Directory",
			"emails": []any{map[string]any{"value": "alice@directory.invalid", "primary": true}}}
		status, raw := a.request(t, http.MethodPost, scimUsers, provisioner.Token, user)
		require.Equal(t, http.StatusCreated, status, string(raw))
		contact = signedIn(a.aliceToken)
		require.Equal(t, "alice@directory.invalid", deref(contact.Email), "undated claims never replace the directory's dated push")
		require.Equal(t, "alice-directory", deref(contact.Username))
		status, raw = a.request(t, http.MethodDelete, scimUsers+"/"+a.alice.ID, provisioner.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		contact = signedIn(a.aliceToken)
		require.True(t, contact == nil || contact.Email == nil && contact.Username == nil, "claims restore no erased contact: %+v", contact)
		paidExternally(t, a, alice, messageSale(t, a), "erased-claims-receipt")
		startFleet(t, a.world)
		log := settledMessages(t, a.world, alice, 1)
		require.Equal(t, billing.MessageNoContact, log[0].Status)
		require.Empty(t, mail.all())
	})
	t.Run("OR-J07-S14/an-asserted-customers-verified-email-is-its-contact-on-a-standalone-server", func(t *testing.T) {
		w := newStandaloneWorld(t)
		alpha := w.Merchants["alpha"]
		status, raw := w.request(t, http.MethodGet, "/v1/me", alpha.CustomerToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		customer, err := w.alpha.GetCustomer(t.Context(), alpha.CustomerID)
		require.NoError(t, err)
		require.NotNil(t, customer.Contact, "the merchant's own customer, whose claims AuthKit keeps under the asserting application")
		require.Equal(t, "alpha@critical.invalid", deref(customer.Contact.Email))
		found, err := w.alpha.ListCustomers(t.Context(), billing.CustomerListParams{Search: "alpha@critical.invalid"})
		require.NoError(t, err)
		require.Len(t, found.Items, 1)
		require.Equal(t, alpha.CustomerID, found.Items[0].ID)
	})
	t.Run("OR-J07-S04/live-host-directory-exposes-no-copied-write-authority", func(t *testing.T) {
		a := newAuthorityWorld(t)
		id := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: id, Currency: "USD", Amount: 1000, Source: "host", SourceID: "live-directory"}})
		require.NoError(t, err)
		customer, err := a.alphaRemote.GetCustomer(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, customer.Contact)
		require.Equal(t, a.alice.Username, *customer.Contact.Username)
		for _, path := range []string{"/v1/app/scim/v2/Users", "/v1/merchant/provisioning-tokens"} {
			status, raw := a.request(t, "POST", path, a.alphaKey.Secret, map[string]any{"externalId": id.String(), "userName": "attacker", "name": "copied"})
			require.Equal(t, 404, status, string(raw))
		}
		after, err := a.alphaRemote.GetCustomer(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, customer.Contact, after.Contact)
		balance(t, a.alpha, id, 1000, 0, 1000)
	})

}
