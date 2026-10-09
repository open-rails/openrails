package scim

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// ServiceProviderConfig serves GET /ServiceProviderConfig (RFC 7643 §5).
func (s *Server) ServiceProviderConfig() http.Handler {
	return s.serve(func(_ context.Context, w http.ResponseWriter, r *http.Request, _ billing.MerchantID) {
		supported := func(ok bool) map[string]bool { return map[string]bool{"supported": ok} }
		writeJSON(w, http.StatusOK, map[string]any{
			"schemas":          []string{ServiceProviderConfigSchema},
			"documentationUri": "https://github.com/open-rails/openrails/blob/master/docs/customer-contacts.md",
			"patch":            supported(true),
			"bulk":             map[string]any{"supported": true, "maxOperations": MaxOperations, "maxPayloadSize": MaxPayloadSize},
			"filter":           map[string]any{"supported": true, "maxResults": MaxResults},
			"changePassword":   supported(false),
			"sort":             supported(false),
			"etag":             supported(false),
			"authenticationSchemes": []map[string]any{{
				"type": "oauthbearertoken", "name": "OAuth Bearer Token", "primary": true,
				"description": "The merchant's provisioning token, or an application credential the deployment's auth accepts, such as a client-credentials access token from the merchant's trusted issuer.",
			}},
			"meta": map[string]string{"resourceType": "ServiceProviderConfig", "location": baseURL(r, "ServiceProviderConfig") + "/ServiceProviderConfig"},
		})
	})
}

func userResourceType(base string) map[string]any {
	return map[string]any{
		"schemas": []string{ResourceTypeSchema}, "id": "User", "name": "User", "endpoint": "/Users",
		"description": "A customer of the merchant, by the host's user id.", "schema": UserSchema,
		"meta": map[string]string{"resourceType": "ResourceType", "location": base + "/ResourceTypes/User"},
	}
}

// ResourceTypes serves GET /ResourceTypes: Users only.
func (s *Server) ResourceTypes() http.Handler {
	return s.serve(func(_ context.Context, w http.ResponseWriter, r *http.Request, _ billing.MerchantID) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schemas": []string{ListResponseSchema}, "totalResults": 1, "startIndex": 1, "itemsPerPage": 1,
			"Resources": []any{userResourceType(baseURL(r, "ResourceTypes"))},
		})
	})
}

// ResourceType serves GET /ResourceTypes/{id}.
func (s *Server) ResourceType() http.Handler {
	return s.serve(func(_ context.Context, w http.ResponseWriter, r *http.Request, _ billing.MerchantID) {
		if !strings.EqualFold(r.PathValue("id"), "User") {
			writeError(w, errorf(http.StatusNotFound, "", "no such resource type"))
			return
		}
		writeJSON(w, http.StatusOK, userResourceType(baseURL(r, "ResourceTypes")))
	})
}

func attribute(name, description string, required bool, mutability, returned string, subs ...map[string]any) map[string]any {
	a := map[string]any{
		"name": name, "type": "string", "multiValued": false, "description": description, "required": required,
		"caseExact": false, "mutability": mutability, "returned": returned, "uniqueness": "none",
	}
	if len(subs) > 0 {
		a["type"], a["subAttributes"] = "complex", subs
	}
	return a
}

func userSchema(base string) map[string]any {
	userName := attribute("userName", "The user's unique name at the merchant's directory.", true, "readWrite", "default")
	userName["uniqueness"] = "server"
	active := attribute("active", "Whether the directory lets the user sign in: shown, never enforced.", false, "readWrite", "default")
	active["type"] = "boolean"
	externalID := attribute("externalId", "The user's id at the host, a UUID: the customer id and the User's id.", true, "immutable", "default")
	externalID["caseExact"] = true
	primary := attribute("primary", "Always true: a customer has one email.", false, "readWrite", "default")
	primary["type"] = "boolean"
	emails := attribute("emails", "The user's email; the primary one, else the first, is kept.", false, "readWrite", "default",
		attribute("value", "An email address.", false, "readWrite", "default"),
		attribute("type", "Accepted and not kept.", false, "writeOnly", "never"),
		primary)
	emails["multiValued"] = true
	return map[string]any{
		"schemas": []string{SchemaSchema}, "id": UserSchema, "name": "User", "description": "A customer of the merchant.",
		"attributes": []any{
			externalID,
			userName,
			attribute("name", "The user's name; formatted is the display name, given and family names compose it when it is absent.", false, "readWrite", "default",
				attribute("formatted", "The display name.", false, "readWrite", "default"),
				attribute("givenName", "Accepted and not kept.", false, "writeOnly", "never"),
				attribute("familyName", "Accepted and not kept.", false, "writeOnly", "never")),
			attribute("displayName", "The display name.", false, "readWrite", "default"),
			emails,
			active,
		},
		"meta": map[string]string{"resourceType": "Schema", "location": base + "/Schemas/" + UserSchema},
	}
}

// Schemas serves GET /Schemas: the User schema.
func (s *Server) Schemas() http.Handler {
	return s.serve(func(_ context.Context, w http.ResponseWriter, r *http.Request, _ billing.MerchantID) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schemas": []string{ListResponseSchema}, "totalResults": 1, "startIndex": 1, "itemsPerPage": 1,
			"Resources": []any{userSchema(baseURL(r, "Schemas"))},
		})
	})
}

// Schema serves GET /Schemas/{id}.
func (s *Server) Schema() http.Handler {
	return s.serve(func(_ context.Context, w http.ResponseWriter, r *http.Request, _ billing.MerchantID) {
		if !strings.EqualFold(r.PathValue("id"), UserSchema) {
			writeError(w, errorf(http.StatusNotFound, "", "no such schema"))
			return
		}
		writeJSON(w, http.StatusOK, userSchema(baseURL(r, "Schemas")))
	})
}
