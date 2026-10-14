package paymentpage

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// railOrigins are the origins each rail's own browser fields load from:
// code, never configuration. Stripe's are its documented Stripe.js sources
// (docs.stripe.com/security/guide#content-security-policy); NMI's are where
// Collect.js may be loaded from (merchants.NMICollectJSURLAllowed).
var railOrigins = []origins{
	{
		script:  []string{"https://*.basistheory.com"},
		frame:   []string{"https://*.basistheory.com"},
		connect: []string{"https://*.basistheory.com"},
	},
	{
		script:  []string{"https://js.stripe.com", "https://*.js.stripe.com"},
		frame:   []string{"https://js.stripe.com", "https://*.js.stripe.com", "https://hooks.stripe.com"},
		connect: []string{stripeapi.APIBase},
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
// (Stripe Checkout refuses iframes too) but an embedded payment page's, which the
// origins its merchant registered frame. Its CSP and X-Frame-Options both
// follow from it.
func frameAncestors(origins []string) string {
	if len(origins) == 0 {
		return "'none'"
	}
	return strings.Join(origins, " ")
}

// Policy is the page's Content-Security-Policy but its frame-ancestors.
type Policy string

// Framed is the whole policy for a page the origins may frame (none: no
// site).
func (p Policy) Framed(origins []string) string {
	return string(p) + "; frame-ancestors " + frameAncestors(origins)
}

// ContentSecurityPolicy is the page's: its own scripts, the rails' field
// origins, the captcha provider's when the deployment challenges (captcha,
// "" for none), the issuers it signs shoppers in at (their token endpoints)
// and any https logo; Framed adds the framing decision.
func ContentSecurityPolicy(captcha string, issuers ...string) Policy {
	all := railOrigins
	if o, ok := captchaOrigins[captcha]; ok {
		all = append(append([]origins{}, all...), o)
	}
	if len(issuers) > 0 {
		all = append(append([]origins{}, all...), origins{connect: issuers})
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
	return Policy(strings.Join([]string{
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
	}, "; "))
}

// Framing answers the origins that may frame a request's page; none frames
// it unless it is an embedded payment page's.
type Framing func(*http.Request) []string

// HeadersHTTP sets the checkout origin's security headers on every answer,
// with policy as its Content-Security-Policy, framed as framing says.
// X-Frame-Options cannot list origins, so a framed page sends none.
func HeadersHTTP(policy Policy, framing Framing) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var origins []string
			if framing != nil {
				origins = framing(r)
			}
			h := w.Header()
			h.Set("Content-Security-Policy", policy.Framed(origins))
			if len(origins) == 0 {
				h.Set("X-Frame-Options", "DENY")
			}
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			// The sign-in return keeps its opener: a framed page's sign-in
			// popup hands its code back to the frame that opened it.
			if r.URL.Path != SignInPath {
				h.Set("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
			}
			h.Set("Permissions-Policy", `payment=(self "https://js.stripe.com" "https://*.js.stripe.com"), camera=(), microphone=(), geolocation=()`)
			h.Set("Server", "")
			next.ServeHTTP(w, r)
		})
	}
}

// SignInPath is where the page's openrails.dev sign-in returns: the
// redirect URI of its client, served the page like an order.
const SignInPath = PagePrefix + "sign-in"

// Page serves the payment page from a build of web/pay: index.html at
// /pay/{order_id} and SignInPath, its files under /assets/, and nothing else.
func Page(assets fs.FS) (http.Handler, error) {
	if assets == nil {
		return nil, errors.New("paymentpage: no payment page build (web/pay)")
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, errors.New("paymentpage: the payment page build has no index.html; build web/pay")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, PagePrefix); ok {
			if _, err := billing.ParseOrderID(rest); r.URL.Path != SignInPath && (err != nil || !strings.HasPrefix(rest, billing.OrderIDPrefix)) {
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
				// #nosec G703 -- fs.ValidPath rejects "..", absolute and empty
				// segments, and assets is an embedded or host-given fs.FS.
				http.ServeFileFS(w, r, assets, "assets/"+rel)
				return
			}
		}
		http.NotFound(w, r)
	}), nil
}
