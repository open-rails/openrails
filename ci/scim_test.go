//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// scimClient drives a SCIM service provider the way a directory does.
type scimClient struct {
	t       *testing.T
	handler http.Handler
	base    string
	token   string
}

func (c scimClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(c.t, json.NewEncoder(&payload).Encode(body))
	}
	r := httptest.NewRequest(method, c.base+path, &payload)
	r.Header.Set("Content-Type", "application/scim+json")
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		require.NoError(c.t, json.Unmarshal(w.Body.Bytes(), &out), "%s %s: %s", method, path, w.Body.String())
		require.True(c.t, strings.HasPrefix(w.Header().Get("Content-Type"), "application/scim+json"), "%s %s answers SCIM", method, path)
	}
	return w.Code, out
}

func (c scimClient) must(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	status, out := c.do(method, path, body)
	require.Equal(c.t, want, status, "%s %s: %v", method, path, out)
	return out
}

func scimUser(id, userName, email, name string) map[string]any {
	u := map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "externalId": id, "userName": userName, "active": true}
	if email != "" {
		u["emails"] = []any{map[string]any{"value": email, "type": "work", "primary": true}}
	}
	if name != "" {
		u["name"] = map[string]any{"formatted": name}
	}
	return u
}

func (c scimClient) find(filter string) []any {
	c.t.Helper()
	out := c.must(http.MethodGet, "/Users?filter="+url.QueryEscape(filter), nil, http.StatusOK)
	resources, _ := out["Resources"].([]any)
	require.EqualValues(c.t, len(resources), out["totalResults"])
	return resources
}

func scimErrorType(out map[string]any) string {
	s, _ := out["scimType"].(string)
	return s
}

// A directory pushes its users to the Client's SCIM handler in process:
// create, change the email by PUT and by PATCH, deactivate, filter, delete;
// the customer read and search show what it pushed.
func TestSCIMHandlerProvisionsCustomerContacts(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "scim-"+uuid.NewString()[:8])
	handler, err := client.SCIMHandler()
	require.NoError(t, err)
	scim := scimClient{t: t, handler: handler}
	ctx := t.Context()

	id := uuid.NewString()
	created := scim.must(http.MethodPost, "/Users", scimUser(id, "ada", "ada@example.test", "Ada Lovelace"), http.StatusCreated)
	require.Equal(t, id, created["id"], "the SCIM id is the host's user id")
	require.Equal(t, id, created["externalId"])
	require.Equal(t, "Ada Lovelace", created["displayName"])
	require.Equal(t, true, created["active"])
	require.Contains(t, created["meta"].(map[string]any)["location"], "/Users/"+id)

	status, out := scim.do(http.MethodPost, "/Users", scimUser(id, "ada", "ada@example.test", ""))
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "uniqueness", scimErrorType(out))
	status, out = scim.do(http.MethodPost, "/Users", scimUser(uuid.NewString(), "ADA", "", ""))
	require.Equal(t, http.StatusConflict, status, "userName is unique, ignoring case")
	require.Equal(t, "uniqueness", scimErrorType(out))
	status, out = scim.do(http.MethodPost, "/Users", scimUser("00u1abcd", "okta-native", "", ""))
	require.Equal(t, http.StatusBadRequest, status, "externalId is the host's UUID")
	require.Equal(t, "invalidValue", scimErrorType(out))
	require.Equal(t, []any{"urn:ietf:params:scim:api:messages:2.0:Error"}, out["schemas"])
	require.Equal(t, "400", out["status"])

	// The customer read and search show the pushed contact.
	customer := billing.CustomerID(uuid.MustParse(id))
	read, err := client.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{customer}})
	require.NoError(t, err)
	require.Len(t, read.Items, 1)
	contact := read.Items[0].Contact
	require.NotNil(t, contact)
	require.Equal(t, "ada@example.test", *contact.Email)
	require.Equal(t, "Ada Lovelace", *contact.Name)
	require.Equal(t, "ada", *contact.Username)
	require.True(t, *contact.Active)
	require.NotNil(t, contact.SyncedAt)
	for _, q := range []string{"ADA@EXAMPLE", "lovelace", "ada"} {
		found, err := client.ListCustomers(ctx, billing.CustomerListParams{Search: q})
		require.NoError(t, err)
		require.Len(t, found.Items, 1, q)
		require.Equal(t, customer, found.Items[0].ID, q)
	}

	// PUT replaces; PATCH changes one attribute (Entra ID spells booleans as strings).
	scim.must(http.MethodPut, "/Users/"+id, scimUser(id, "ada", "countess@example.test", "Ada King"), http.StatusOK)
	require.Len(t, scim.find(`emails.value eq "countess@example.test"`), 1)
	require.Empty(t, scim.find(`emails.value eq "ada@example.test"`))
	patched := scim.must(http.MethodPatch, "/Users/"+id, map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []any{
			map[string]any{"op": "Replace", "path": `emails[type eq "work"].value`, "value": "ada.king@example.test"},
			map[string]any{"op": "replace", "path": "active", "value": "False"},
		},
	}, http.StatusOK)
	require.Equal(t, false, patched["active"])
	require.Equal(t, "ada.king@example.test", patched["emails"].([]any)[0].(map[string]any)["value"])
	read, err = client.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{customer}})
	require.NoError(t, err)
	require.Equal(t, "ada.king@example.test", *read.Items[0].Contact.Email)
	require.False(t, *read.Items[0].Contact.Active, "active is shown")

	// Filters, and a list without one pages the whole directory.
	other := uuid.NewString()
	scim.must(http.MethodPost, "/Users", scimUser(other, "alan", "alan@example.test", ""), http.StatusCreated)
	require.Len(t, scim.find(`externalId eq "`+id+`"`), 1)
	require.Len(t, scim.find(`userName eq "ALAN"`), 1)
	require.Empty(t, scim.find(`externalId eq "not-a-uuid"`))
	status, out = scim.do(http.MethodGet, "/Users?filter="+url.QueryEscape(`displayName co "Ada"`), nil)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "invalidFilter", scimErrorType(out))
	page := scim.must(http.MethodGet, "/Users?startIndex=1&count=1", nil, http.StatusOK)
	require.EqualValues(t, 2, page["totalResults"])
	require.EqualValues(t, 1, page["itemsPerPage"])
	require.Equal(t, id, page["Resources"].([]any)[0].(map[string]any)["id"], "oldest first")
	page = scim.must(http.MethodGet, "/Users?startIndex=2&count=1", nil, http.StatusOK)
	require.Equal(t, other, page["Resources"].([]any)[0].(map[string]any)["id"])

	// DELETE erases the contact and keeps the billing record.
	scim.must(http.MethodDelete, "/Users/"+id, nil, http.StatusNoContent)
	scim.must(http.MethodGet, "/Users/"+id, nil, http.StatusNotFound)
	read, err = client.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{customer}})
	require.NoError(t, err)
	require.Len(t, read.Items, 1, "the customer stays")
	require.Nil(t, read.Items[0].Contact, "its contact is erased")
	stale := scimUser(id, "ada", "ada@example.test", "")
	stale["meta"] = map[string]any{"lastModified": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	scim.must(http.MethodPost, "/Users", stale, http.StatusCreated)
	read, err = client.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{customer}})
	require.NoError(t, err)
	require.Nil(t, read.Items[0].Contact.Email, "a report older than the erasure restores no value")

	// Discovery.
	config := scim.must(http.MethodGet, "/ServiceProviderConfig", nil, http.StatusOK)
	require.Equal(t, map[string]any{"supported": true, "maxOperations": float64(1000), "maxPayloadSize": float64(1 << 20)}, config["bulk"])
	require.Equal(t, true, config["patch"].(map[string]any)["supported"])
	types := scim.must(http.MethodGet, "/ResourceTypes", nil, http.StatusOK)
	require.EqualValues(t, 1, types["totalResults"])
	scim.must(http.MethodGet, "/Schemas/urn:ietf:params:scim:schemas:core:2.0:User", nil, http.StatusOK)
}

// A Bulk batch applies each operation on its own and reports each: creates,
// an email change, a deactivation and a delete, with one refused operation
// that stops nothing.
func TestSCIMBulk(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "scim-bulk-"+uuid.NewString()[:8])
	handler, err := client.SCIMHandler()
	require.NoError(t, err)
	scim := scimClient{t: t, handler: handler}

	gone := uuid.NewString()
	scim.must(http.MethodPost, "/Users", scimUser(gone, "gone", "gone@example.test", ""), http.StatusCreated)
	a, b := uuid.NewString(), uuid.NewString()
	modified := time.Now().UTC().Format(time.RFC3339)
	userA := scimUser(a, "grace", "grace@example.test", "Grace Hopper")
	userA["meta"] = map[string]any{"lastModified": modified}
	bulk := map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:BulkRequest"},
		"Operations": []any{
			map[string]any{"method": "POST", "bulkId": "a", "path": "/Users", "data": userA},
			map[string]any{"method": "POST", "bulkId": "b", "path": "/Users", "data": scimUser(b, "barbara", "barbara@example.test", "")},
			map[string]any{"method": "POST", "bulkId": "bad", "path": "/Users", "data": scimUser("not-a-uuid", "bad", "", "")},
			map[string]any{"method": "PUT", "path": "/Users/bulkId:a", "data": scimUser(a, "grace", "admiral@example.test", "Grace Hopper")},
			map[string]any{"method": "PATCH", "path": "/Users/" + b, "data": map[string]any{
				"schemas":    []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
				"Operations": []any{map[string]any{"op": "replace", "value": map[string]any{"active": false}}},
			}},
			map[string]any{"method": "DELETE", "path": "/Users/" + gone},
		},
	}
	out := scim.must(http.MethodPost, "/Bulk", bulk, http.StatusOK)
	require.Equal(t, []any{"urn:ietf:params:scim:api:messages:2.0:BulkResponse"}, out["schemas"])
	var statuses []string
	for _, op := range out["Operations"].([]any) {
		statuses = append(statuses, op.(map[string]any)["status"].(string))
	}
	require.Equal(t, []string{"201", "201", "400", "200", "200", "204"}, statuses)
	refused := out["Operations"].([]any)[2].(map[string]any)
	require.Equal(t, "bad", refused["bulkId"])
	require.Equal(t, "invalidValue", refused["response"].(map[string]any)["scimType"])

	user := scim.must(http.MethodGet, "/Users/"+a, nil, http.StatusOK)
	require.Equal(t, "admiral@example.test", user["emails"].([]any)[0].(map[string]any)["value"])
	user = scim.must(http.MethodGet, "/Users/"+b, nil, http.StatusOK)
	require.Equal(t, false, user["active"])
	scim.must(http.MethodGet, "/Users/"+gone, nil, http.StatusNotFound)

	// failOnErrors stops after that many errors.
	bulk = map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:BulkRequest"}, "failOnErrors": 1,
		"Operations": []any{
			map[string]any{"method": "DELETE", "path": "/Users/" + gone},
			map[string]any{"method": "POST", "bulkId": "late", "path": "/Users", "data": scimUser(uuid.NewString(), "late", "", "")},
		},
	}
	out = scim.must(http.MethodPost, "/Bulk", bulk, http.StatusOK)
	require.Len(t, out["Operations"], 1)
	require.Equal(t, "404", out["Operations"].([]any)[0].(map[string]any)["status"])
	require.Empty(t, scim.find(`userName eq "late"`))

	// A batch beyond the advertised limit is refused whole.
	var ops []any
	for range 1001 {
		ops = append(ops, map[string]any{"method": "DELETE", "path": "/Users/" + uuid.NewString()})
	}
	status, refusal := scim.do(http.MethodPost, "/Bulk", map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:BulkRequest"}, "Operations": ops})
	require.Equal(t, http.StatusRequestEntityTooLarge, status)
	require.Equal(t, "tooLarge", scimErrorType(refusal))
}

// Provisioning mounted on an embedded host: each merchant's directory
// authenticates with its own token, which reaches only its own customers; a
// revoked or unknown token is refused, and a merchant's declared token works.
func TestSCIMProvisioningTokensIsolateMerchants(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	mount := func(slug, declared string) (*openrails.Client, http.Handler) {
		cfg := f.config()
		cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug, Secrets: openrails.MerchantSecrets{SCIMToken: declared}}
		client, err := openrails.New(ctx, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, client.Close(t.Context())) })
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, client, openrails.Routes{Auth: authtest.Deny{}, Prefix: "/billing", Provisioning: true}))
		return client, mux
	}
	declared := "declared-" + strings.Repeat("x", 32)
	alice, aliceMux := mount("scim-a-"+uuid.NewString()[:8], declared)
	bob, bobMux := mount("scim-b-"+uuid.NewString()[:8], "")

	minted, err := alice.CreateProvisioningToken(ctx, billing.CreateProvisioningTokenParams{Name: "AuthKit"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(minted.Token, "orscim_"))
	bobToken, err := bob.CreateProvisioningToken(ctx, billing.CreateProvisioningTokenParams{Name: "Okta"})
	require.NoError(t, err)
	tokens, err := alice.ListProvisioningTokens(ctx, billing.ProvisioningTokenListParams{})
	require.NoError(t, err)
	require.Len(t, tokens.Items, 2, "the declared token and the minted one")
	for _, tok := range tokens.Items {
		raw, err := json.Marshal(tok)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "orscim_", "a token is shown once")
	}

	asAlice := scimClient{t: t, handler: aliceMux, base: "/billing/scim/v2", token: minted.Token}
	asBob := scimClient{t: t, handler: bobMux, base: "/billing/scim/v2", token: bobToken.Token}
	shared := uuid.NewString()
	asAlice.must(http.MethodPost, "/Users", scimUser(shared, "shared", "alice-side@example.test", ""), http.StatusCreated)
	asBob.must(http.MethodGet, "/Users/"+shared, nil, http.StatusNotFound)
	require.Empty(t, asBob.find(`externalId eq "`+shared+`"`))
	asBob.must(http.MethodPost, "/Users", scimUser(shared, "shared", "bob-side@example.test", ""), http.StatusCreated)
	asBob.must(http.MethodDelete, "/Users/"+shared, nil, http.StatusNoContent)
	user := asAlice.must(http.MethodGet, "/Users/"+shared, nil, http.StatusOK)
	require.Equal(t, "alice-side@example.test", user["emails"].([]any)[0].(map[string]any)["value"], "one merchant's push never touches another's")

	for name, token := range map[string]string{"none": "", "unknown": "orscim_" + uuid.NewString(), "another merchant's": bobToken.Token} {
		status, out := scimClient{t: t, handler: aliceMux, base: "/billing/scim/v2", token: token}.do(http.MethodGet, "/Users/"+shared, nil)
		require.Equal(t, http.StatusUnauthorized, status, name)
		require.Equal(t, "401", out["status"], name)
	}
	asDeclared := scimClient{t: t, handler: aliceMux, base: "/billing/scim/v2", token: declared}
	asDeclared.must(http.MethodGet, "/Users/"+shared, nil, http.StatusOK)

	require.NoError(t, alice.DeleteProvisioningToken(ctx, minted.ID))
	status, _ := asAlice.do(http.MethodGet, "/Users/"+shared, nil)
	require.Equal(t, http.StatusUnauthorized, status, "a revoked token is refused at once")
	require.ErrorIs(t, alice.DeleteProvisioningToken(ctx, minted.ID), billing.ErrNotFound)
}
