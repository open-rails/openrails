package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/history"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/server/internal/adminconsole"
)

type consoleMerchantPage struct {
	Data []adminconsole.Merchant `json:"data"`
	Next *string                 `json:"next_cursor"`
	Root bool                    `json:"root"`
}

func (s *Server) consoleMerchants(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// A browser explicitly sends its credential and any sender proof. An
	// ambient account cookie alone never opens a directory read.
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		consoleRefuse(w, r, auth.ErrUnauthenticated)
		return
	}
	verified, err := s.auth.Authenticate(r)
	if err != nil || verified == nil {
		consoleRefuse(w, r, err)
		return
	}
	checker, ok := verified.(auth.PermissionChecker)
	rootAuthority, hasRoot := s.auth.(auth.Root)
	if !ok || !hasRoot {
		consoleRefuse(w, r, auth.ErrForbidden)
		return
	}
	root, err := rootAuthority.RootScope(r.Context())
	if err != nil {
		consoleRefuse(w, r, err)
		return
	}
	out := consoleMerchantPage{Data: []adminconsole.Merchant{}}
	out.Root, err = s.consoleStaff(r.Context(), checker, root, "root")
	if err != nil {
		consoleRefuse(w, r, err)
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if _, _, err := pagination.After(cursor); err != nil {
		billingauth.WriteJSONError(w, http.StatusBadRequest, billing.CodeInvalidCursor, "invalid directory cursor")
		return
	}
	if out.Root {
		page, err := s.controlPlane.ListActiveMerchantIDs(r.Context(), billing.PageRequest{Limit: 100, Cursor: cursor})
		if err != nil {
			consoleRefuse(w, r, err)
			return
		}
		for _, id := range page.Items {
			merchant, err := s.merchants.Get(r.Context(), id)
			if err != nil {
				consoleRefuse(w, r, err)
				return
			}
			out.Data = append(out.Data, adminconsole.Merchant{ID: id.String(), Slug: merchant.Slug, DisplayName: s.merchants.DisplayName(r.Context(), id)})
		}
		if page.Next != "" {
			out.Next = &page.Next
		}
	} else if bound, ok := verified.(auth.Bound); ok && bound.BoundScope() != (auth.Scope{}) {
		scope := bound.BoundScope()
		merchant, found, err := s.controlPlane.MerchantOfScope(r.Context(), scope)
		if err != nil {
			consoleRefuse(w, r, err)
			return
		}
		if !found {
			consoleRefuse(w, r, auth.ErrForbidden)
			return
		}
		allowed, err := s.consoleStaff(r.Context(), checker, scope, "merchant")
		if err != nil || !allowed {
			if err == nil {
				err = auth.ErrForbidden
			}
			consoleRefuse(w, r, err)
			return
		}
		out.Data = append(out.Data, adminconsole.Merchant{ID: merchant.ID.String(), Slug: merchant.Slug, DisplayName: s.merchants.DisplayName(r.Context(), merchant.ID)})
	} else {
		who := verified.Identity()
		if who.Issuer != s.controlPlane.Issuer() || !billingauth.Interactive(who) {
			consoleRefuse(w, r, auth.ErrForbidden)
			return
		}
		memberships, err := s.controlPlane.ListUserMerchants(r.Context(), who.Subject)
		if err != nil {
			consoleRefuse(w, r, err)
			return
		}
		for _, merchant := range memberships {
			scope, err := s.controlPlane.MerchantScope(r.Context(), merchant.ID)
			if err != nil {
				consoleRefuse(w, r, err)
				return
			}
			allowed, err := s.consoleStaff(r.Context(), checker, scope, "merchant")
			if err != nil {
				consoleRefuse(w, r, err)
				return
			}
			if allowed {
				out.Data = append(out.Data, adminconsole.Merchant{ID: merchant.ID.String(), Slug: merchant.Slug, DisplayName: merchant.DisplayName, Role: merchant.Role})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// Only a permission the verified credential currently holds opens a staff
// directory. Membership is a candidate list, never an authority bypass for a
// scoped token or a key. Root grants follow the same credential ceilings.
// Clearing destructive holds alone opens none.
func (s *Server) consoleStaff(ctx context.Context, checker auth.PermissionChecker, scope auth.Scope, persona string) (bool, error) {
	permissions := []string{}
	for _, permission := range billing.Permissions() {
		if permission != billing.RootAdminRateLimitsUnlock && strings.HasPrefix(string(permission), persona+":") {
			permissions = append(permissions, string(permission))
		}
	}
	if persona == "root" {
		permissions = append(permissions, string(billing.RootMerchantsRead))
	}
	for _, permission := range []string{"members:read", "members:manage", "roles:read", "roles:manage", "credentials:read", "credentials:manage"} {
		permissions = append(permissions, persona+":"+permission)
	}
	catalog, hasCatalog := s.auth.(auth.PermissionCatalog)
	for _, permission := range permissions {
		if hasCatalog && !catalog.KnownPermission(permission) {
			continue
		}
		allowed, err := checker.Can(ctx, scope, permission)
		if err != nil || allowed {
			return allowed, err
		}
	}
	return false, nil
}

func consoleRefuse(w http.ResponseWriter, r *http.Request, err error) {
	refusal := auth.Refuse(r, err)
	for name, values := range refusal.Header {
		w.Header()[name] = values
	}
	message := http.StatusText(refusal.Status)
	switch {
	case errors.Is(err, auth.ErrRevoked):
		billingauth.WriteJSONError(w, refusal.Status, billing.CodeCredentialRevoked, message)
	case errors.Is(err, auth.ErrExpired):
		billingauth.WriteJSONError(w, refusal.Status, billing.CodeCredentialExpired, message)
	case refusal.Status == http.StatusForbidden:
		billingauth.WriteJSONError(w, refusal.Status, billing.CodePermissionRequired, message)
	case refusal.Status == http.StatusUnauthorized:
		billingauth.WriteJSONError(w, refusal.Status, billing.CodeAuthenticationRequired, message)
	default:
		billingauth.WriteJSONError(w, refusal.Status, billing.CodeAuthenticationUnavailable, message)
	}
}

// consoleNewMerchant is a merchant the console's New merchant creates: its
// name and, where Vault holds configuration, the name buyers see.
type consoleNewMerchant struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// consoleCreateMerchant is the console's New merchant: a root operator's
// own session holding root:merchants:create creates a merchant through the
// operator's provisioning and owns its team. It is the standalone product's
// route, mounted with the console; the engine's route catalog registers no
// merchant.
func (s *Server) consoleCreateMerchant(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		consoleRefuse(w, r, auth.ErrUnauthenticated)
		return
	}
	verified, err := s.auth.Authenticate(r)
	if err != nil || verified == nil {
		consoleRefuse(w, r, err)
		return
	}
	who := verified.Identity()
	checker, ok := verified.(auth.PermissionChecker)
	rootAuthority, hasRoot := s.auth.(auth.Root)
	// A person's own session at this server: the owner is an account here,
	// and a credential bound to a merchant reaches no other.
	if bound, isBound := verified.(auth.Bound); !ok || !hasRoot || isBound && bound.BoundScope() != (auth.Scope{}) ||
		who.Issuer != s.controlPlane.Issuer() || !billingauth.Interactive(who) {
		consoleRefuse(w, r, auth.ErrForbidden)
		return
	}
	root, err := rootAuthority.RootScope(r.Context())
	if err != nil {
		consoleRefuse(w, r, err)
		return
	}
	allowed, err := checker.Can(r.Context(), root, string(billing.RootMerchantsCreate))
	if err != nil || !allowed {
		if err == nil {
			err = auth.ErrForbidden
		}
		consoleRefuse(w, r, err)
		return
	}
	var req consoleNewMerchant
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		if field, unknown := strings.CutPrefix(err.Error(), "json: unknown field "); unknown {
			consoleError(w, api.Coded(billing.CodeUnknownField, "").WithParam(strings.Trim(field, `"`)))
			return
		}
		consoleError(w, api.Coded(billing.CodeInvalidRequestBody, "the body is one JSON object: name and display_name"))
		return
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	// A display name is configuration: refused before anything is created
	// where a file holds it.
	if req.DisplayName != "" && !s.runtime.MerchantConfigManageable() {
		consoleError(w, api.Coded(billing.CodeMerchantConfigReadOnly, "merchant configuration is read from a file: declare the display name there").WithParam("display_name"))
		return
	}
	// The display name's history entry is the console's; the log line names who.
	ctx := history.WithSource(r.Context(), billing.LogSourceConsole)
	res, err := s.provision(ctx, billing.ProvisionMerchantParams{Slug: req.Name, DisplayName: req.DisplayName, OwnerUserID: who.Subject})
	switch {
	case errors.Is(err, billing.ErrInvalidMerchantSlug):
		consoleError(w, api.Coded(billing.CodeInvalidParam, "a merchant's name is lowercase letters, digits and hyphens").WithParam("name"))
		return
	case errors.Is(err, billing.ErrMerchantSlugReserved):
		consoleError(w, api.Coded(billing.CodeInvalidParam, "that name is reserved").WithParam("name"))
		return
	case errors.Is(err, billing.ErrMerchantNameTaken):
		consoleError(w, api.Coded(billing.CodeResourceConflict, "another merchant holds that name").WithParam("name"))
		return
	case errors.Is(err, billing.ErrMerchantCreationRefused), errors.Is(err, billing.ErrMerchantCreationEmailUnverified),
		errors.Is(err, billing.ErrMerchantCreationPaymentMethodRequired):
		consoleError(w, api.Coded(billing.CodePermissionRequired, "the deployment's merchant creation policy refuses it: "+err.Error()))
		return
	case err != nil:
		s.Log.ErrorContext(r.Context(), "console: merchant not created", logx.Err(err), "subject", who.Subject)
		consoleError(w, api.Coded(billing.CodeInternalError, "the merchant could not be created"))
		return
	case !res.Created:
		consoleError(w, api.Coded(billing.CodeResourceConflict, "another merchant holds that name").WithParam("name"))
		return
	}
	s.Log.InfoContext(r.Context(), "console: merchant created", "merchant_id", res.MerchantID.String(), "subject", who.Subject)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(adminconsole.Merchant{ID: res.MerchantID.String(), Slug: billing.NormalizeMerchantSlug(req.Name),
		DisplayName: s.merchants.DisplayName(r.Context(), res.MerchantID), Role: "owner"})
}

func consoleError(w http.ResponseWriter, refusal *api.APIError) {
	if id := strings.TrimSpace(w.Header().Get("X-Request-ID")); id != "" {
		refusal.WithRequestID(id)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(refusal.HTTPStatus)
	_ = json.NewEncoder(w).Encode(refusal.ToResponse())
}
