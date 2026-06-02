package middleware

import (
	"crypto/x509"
	"strings"

	"github.com/doujins-org/ginapi/response"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	ServiceIdentityContextKey = "service_identity"
	ServiceScopesContextKey   = "service_scopes"

	ServiceScopeAll              = "*"
	ServiceScopeEntitlementsRead = "entitlements:read"
	ServiceScopeCreditsRead      = "credits:read"
	ServiceScopeCreditsWrite     = "credits:write"
	ServiceScopeCreditTypesRead  = "credit_types:read"
	ServiceScopeCreditTypesWrite = "credit_types:write"
)

// ServiceMTLSRequired requires a verified client certificate whose identity is
// present in clientScopes. Identities are matched exactly against URI SANs,
// DNS SANs, and, as a legacy certificate-shape fallback, Subject CN.
func ServiceMTLSRequired(clientScopes map[string][]string) gin.HandlerFunc {
	clients := makeClientScopeSet(clientScopes)
	return func(c *gin.Context) {
		if len(clients) == 0 {
			log.Warn("service mTLS middleware misconfigured: no allowed identities")
			response.InternalError(c, "service mTLS authentication not configured")
			c.Abort()
			return
		}
		if c.Request == nil || c.Request.TLS == nil || len(c.Request.TLS.PeerCertificates) == 0 {
			response.UnauthorizedWithMessage(c, "mTLS client certificate required")
			c.Abort()
			return
		}

		cert := c.Request.TLS.PeerCertificates[0]
		identity, ok := matchCertificateIdentity(cert, clients)
		if !ok {
			log.WithField("subject", cert.Subject.String()).Warn("service mTLS peer identity rejected")
			response.ForbiddenWithMessage(c, "service_identity_not_allowed")
			c.Abort()
			return
		}

		c.Set(ServiceIdentityContextKey, identity)
		c.Set(ServiceScopesContextKey, clients[identity])
		c.Next()
	}
}

// RequireServiceScope requires the authenticated service identity to have a
// specific scope. The "*" scope grants all service-route access.
func RequireServiceScope(scope string) gin.HandlerFunc {
	scope = strings.TrimSpace(scope)
	return func(c *gin.Context) {
		value, ok := c.Get(ServiceScopesContextKey)
		if !ok {
			response.UnauthorizedWithMessage(c, "service identity required")
			c.Abort()
			return
		}
		scopes, ok := value.(map[string]struct{})
		if !ok {
			response.InternalError(c, "service scope state invalid")
			c.Abort()
			return
		}
		if _, ok := scopes[ServiceScopeAll]; ok {
			c.Next()
			return
		}
		if _, ok := scopes[scope]; ok {
			c.Next()
			return
		}
		response.ForbiddenWithMessage(c, "service_scope_required")
		c.Abort()
	}
}

func makeClientScopeSet(values map[string][]string) map[string]map[string]struct{} {
	clients := make(map[string]map[string]struct{}, len(values))
	for identity, scopes := range values {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			continue
		}
		scopeSet := make(map[string]struct{}, len(scopes))
		for _, scope := range scopes {
			scope = strings.TrimSpace(scope)
			if scope != "" {
				scopeSet[scope] = struct{}{}
			}
		}
		clients[identity] = scopeSet
	}
	return clients
}

func matchCertificateIdentity(cert *x509.Certificate, clients map[string]map[string]struct{}) (string, bool) {
	if cert == nil {
		return "", false
	}
	for _, uri := range cert.URIs {
		if uri == nil {
			continue
		}
		if _, ok := clients[uri.String()]; ok {
			return uri.String(), true
		}
	}
	for _, dnsName := range cert.DNSNames {
		dnsName = strings.TrimSpace(dnsName)
		if _, ok := clients[dnsName]; ok {
			return dnsName, true
		}
	}
	commonName := strings.TrimSpace(cert.Subject.CommonName)
	if _, ok := clients[commonName]; ok {
		return commonName, true
	}
	return "", false
}
