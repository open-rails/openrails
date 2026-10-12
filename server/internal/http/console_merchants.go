package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
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
func (s *Server) consoleStaff(ctx context.Context, checker auth.PermissionChecker, scope auth.Scope, persona string) (bool, error) {
	permissions := []string{}
	for _, permission := range billing.Permissions() {
		if strings.HasPrefix(string(permission), persona+":") {
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
	code := billing.CodeAuthenticationUnavailable
	if refusal.Status == http.StatusUnauthorized {
		code = billing.CodeAuthenticationRequired
	}
	if refusal.Status == http.StatusForbidden {
		code = billing.CodePermissionRequired
	}
	if errors.Is(err, auth.ErrExpired) {
		code = billing.CodeCredentialExpired
	}
	if errors.Is(err, auth.ErrRevoked) {
		code = billing.CodeCredentialRevoked
	}
	billingauth.WriteJSONError(w, refusal.Status, code, http.StatusText(refusal.Status))
}
