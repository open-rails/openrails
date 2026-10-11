package hostedcheckout

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// railOrigins are the origins each rail's own browser fields load from:
// code, never configuration. Stripe's are its documented Stripe.js sources
// (docs.stripe.com/security/guide#content-security-policy); NMI's are where
// Collect.js may be loaded from (merchants.NMICollectJSURLAllowed).
var railOrigins = []origins{
	{
		script:  []string{"https://js.stripe.com", "https://*.js.stripe.com"},
		frame:   []string{"https://js.stripe.com", "https://*.js.stripe.com", "https://hooks.stripe.com"},
		connect: []string{"https://api.stripe.com"},
	},
	{
		script:  nmiOrigins,
		frame:   nmiOrigins,
		connect: nmiOrigins,
		style:   nmiOrigins,
		font:    nmiOrigins,
	},
}

var nmiOrigins = []string{"https://secure.networkmerchants.com", "https://secure.nmi.com"}

type origins struct{ script, frame, connect, style, font []string }

// captchaOrigins are where each captcha provider's widget loads from
// (config.CaptchaScriptURL), for a deployment that challenges.
var captchaOrigins = map[string]origins{
	config.CaptchaProviderTurnstile: {script: []string{"https://challenges.cloudflare.com"}, frame: []string{"https://challenges.cloudflare.com"}},
	config.CaptchaProviderHCaptcha: {
		script: []string{"https://js.hcaptcha.com", "https://*.hcaptcha.com"}, frame: []string{"https://*.hcaptcha.com"},
		connect: []string{"https://*.hcaptcha.com"}, style: []string{"https://*.hcaptcha.com"},
	},
	config.CaptchaProviderRecaptchaV3: {
		script: []string{"https://www.google.com", "https://www.gstatic.com"}, frame: []string{"https://www.google.com"},
		connect: []string{"https://www.google.com"},
	},
}

// frameAncestors is the page's one framing decision: no site frames it
// (Stripe Checkout refuses iframes too). Its CSP and X-Frame-Options both
// follow from it.
const frameAncestors = "'none'"

// ContentSecurityPolicy is the page's: its own scripts, the rails' field
// origins, the captcha provider's when the deployment challenges (captcha,
// "" for none), any https logo, and the framing decision.
func ContentSecurityPolicy(captcha string) string {
	all := railOrigins
	if o, ok := captchaOrigins[captcha]; ok {
		all = append(append([]origins{}, all...), o)
	}
	var script, frame, connect, style, font []string
	for _, o := range all {
		script, frame, connect = append(script, o.script...), append(frame, o.frame...), append(connect, o.connect...)
		style, font = append(style, o.style...), append(font, o.font...)
	}
	directive := func(name string, sources ...[]string) string {
		var out []string
		for _, s := range sources {
			out = append(out, s...)
		}
		return name + " " + strings.Join(out, " ")
	}
	return strings.Join([]string{
		"default-src 'none'",
		directive("script-src", []string{"'self'"}, script),
		directive("frame-src", frame),
		directive("connect-src", []string{"'self'"}, connect),
		directive("style-src", []string{"'self'", "'unsafe-inline'"}, style),
		directive("img-src", []string{"'self'", "data:", "https:"}),
		directive("font-src", []string{"'self'"}, font),
		"form-action 'none'",
		"base-uri 'none'",
		"object-src 'none'",
		"frame-ancestors " + frameAncestors,
	}, "; ")
}

// HeadersHTTP sets the checkout host's security headers on every answer,
// with policy as its Content-Security-Policy.
func HeadersHTTP(policy string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", policy)
			if frameAncestors == "'none'" {
				h.Set("X-Frame-Options", "DENY")
			}
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
			h.Set("Permissions-Policy", `payment=(self "https://js.stripe.com"), camera=(), microphone=(), geolocation=()`)
			h.Set("Server", "")
			next.ServeHTTP(w, r)
		})
	}
}

// Page serves the checkout page from a build of web/checkout: index.html at
// /c/{order_id}, its files under /assets/, and nothing else.
func Page(assets fs.FS) (http.Handler, error) {
	if assets == nil {
		return nil, errors.New("hostedcheckout: no checkout page build (web/checkout)")
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, errors.New("hostedcheckout: the checkout page build has no index.html; build web/checkout")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/c/"); ok {
			if _, err := billing.ParseOrderID(rest); err != nil || !strings.HasPrefix(rest, billing.OrderIDPrefix) {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(index)
			return
		}
		if rel, ok := strings.CutPrefix(r.URL.Path, "/assets/"); ok && rel != "" && fs.ValidPath("assets/"+rel) {
			if info, err := fs.Stat(assets, "assets/"+rel); err == nil && !info.IsDir() {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				http.ServeFileFS(w, r, assets, "assets/"+rel)
				return
			}
		}
		http.NotFound(w, r)
	}), nil
}
