package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/open-rails/openrails/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// checkoutConfigMaxAge is how long a browser may reuse this document. Arming a
// PSP is an operator action measured in minutes, and a stale answer only ever
// costs one failed checkout that a reload fixes — but an ETag makes the common
// case a 304 anyway, so the window stays short.
const checkoutConfigMaxAge = "public, max-age=60"

// GetCheckoutConfig serves the PUBLIC per-merchant checkout discovery document
// (#829): the merchant's armed PSPs, and for each the public-by-nature values a
// browser needs to tokenize on it.
//
// Unauthenticated by design — every value it can serve is already public (an
// NMI Collect.js key, a Basis Theory public application key). That is enforced
// structurally by merchants.publicRailProfiles, a whitelist of settings keys:
// merchant SECRETS live in the secret store, which this path never opens, and a
// settings key that is not on the whitelist cannot reach the response at all.
//
// The merchant is whichever one the request resolved to — the Host/api_host
// mechanism (#734) that already governs every public route, applied by the
// server's middleware chain before the handler runs.
func GetCheckoutConfig(r *httprequest.Request) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorJSON(http.StatusNotFound, "no merchant for this host")
		return
	}
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "merchant configuration unavailable")
		return
	}

	env := config.ExpectedProviderEnvironment(r.State.Config != nil && r.State.Config.IsTestMode())
	psps, err := r.State.Merchants.PublicCheckoutPSPs(r.Request.Context(), mid, env)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load checkout configuration")
		return
	}

	body := merchants.PublicCheckoutConfig{Object: "checkout_config", PSPs: psps}
	encoded, err := json.Marshal(body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to encode checkout configuration")
		return
	}

	// max-age carries the caching: a browser rendering checkout repeatedly
	// reuses the document with no round trip at all. The ETag is content-derived
	// so a shared cache can still revalidate; the neutral request transport has
	// no bodyless-response primitive, so this handler does not itself answer 304
	// (a revalidation gets a normal 200 with the same ETag, which is correct).
	sum := sha256.Sum256(encoded)
	r.SetHeader("Cache-Control", checkoutConfigMaxAge)
	r.SetHeader("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	r.SuccessJSON(body)
}
