package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/pkg/authprovider"
)

const (
	rateLimitScopeIP   = "ip"
	rateLimitScopeUser = "user"
)

type rateLimitSubject struct {
	Scope string
	Value string
	Key   string
}

// RateLimitSubjectKeysHTTP is the gin-free analogue of RateLimitSubjectKeys
// (issue #282): it derives the same ip:/user: subject keys from a plain
// *http.Request, reading identity from the request context (billingauth). The
// embedded surface uses this to expose rate-limit subjects without importing gin.
func RateLimitSubjectKeysHTTP(r *http.Request) []string {
	if r == nil {
		return nil
	}
	subjects := make([]rateLimitSubject, 0, 2)
	clientIP := strings.TrimSpace(httpClientIP(r))
	if clientIP != "" {
		subjects = append(subjects, rateLimitSubject{Scope: rateLimitScopeIP, Value: clientIP, Key: rateLimitScopeIP + ":" + clientIP})
	}
	if uc, ok := authprovider.FromContext(r.Context()); ok {
		userID := strings.TrimSpace(uc.UserID)
		if userID != "" {
			subjects = append(subjects, rateLimitSubject{Scope: rateLimitScopeUser, Value: userID, Key: rateLimitScopeUser + ":" + userID})
		}
	}
	return subjectKeys(subjects)
}

// httpClientIP extracts the socket peer IP from a plain request (no forwarded
// header trust), matching the gin ClientIP used by rate-limit subjects.
func httpClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func subjectKeys(subjects []rateLimitSubject) []string {
	keys := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		if subject.Key != "" {
			keys = append(keys, subject.Key)
		}
	}
	return keys
}
