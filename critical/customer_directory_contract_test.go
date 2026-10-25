//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
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
		w.command(t, map[string]any{"Op": "customer-token", "Merchant": "alpha", "ID": id}, &token)
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
		bad := map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": []any{map[string]any{"op": "replace", "path": "displayName", "value": "Must Roll Back"}, map[string]any{"op": "replace", "path": "externalId", "value": "new-owner"}}}
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
}
