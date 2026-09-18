package middleware

import (
	"net/http"
	"path"
	"strings"

	archiveformat "github.com/open-rails/openrails/internal/merchantarchive/format"
)

// IsMerchantBillingArchive recognizes the fixed archive route beneath an
// optional host mount prefix. The route still requires its merchant permission;
// matching here grants no authority and exempts no other route from body caps.
func IsMerchantBillingArchive(r *http.Request) bool {
	return r != nil && r.URL != nil && r.URL.Path == path.Clean(r.URL.Path) && r.URL.RawPath == "" && strings.HasSuffix(r.URL.Path, "/v1/merchant/billing-archive")
}

func isMerchantArchiveImport(r *http.Request) bool {
	return IsMerchantBillingArchive(r) && r.Method == http.MethodPost
}

func requestBodyLimit(r *http.Request, ordinary int64) int64 {
	if isMerchantArchiveImport(r) && ordinary == DefaultMaxBodyBytes {
		return archiveformat.MaxBytes
	}
	return ordinary
}
