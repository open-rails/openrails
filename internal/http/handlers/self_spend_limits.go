package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// The delegated invoker's own spend-window read (or#930). OpenRails enforces
// per-invoker spend windows on every admission; before this, the only signal an
// invoker got about its budget was a denial at submit time. This returns the
// windows it is actually metered against, with their live totals — a read over
// the accounting admission already keeps, not a second one.

type selfSpendLimitsDocument struct {
	Currency string            `json:"currency"`
	Invoker  string            `json:"invoker"`
	Windows  []selfSpendWindow `json:"windows"`
}

// selfSpendWindow is one window. Windows are estimate-based, so Used already
// includes in-flight reservations and Reserved names that part — what a release
// would hand back. ResetsAt is the window's real fixed boundary.
type selfSpendWindow struct {
	Scope         string    `json:"scope"`
	Key           string    `json:"key"`
	WindowSeconds int64     `json:"window_seconds"`
	Limit         int64     `json:"limit"`
	Currency      string    `json:"currency"`
	Used          int64     `json:"used"`
	Reserved      int64     `json:"reserved"`
	Remaining     int64     `json:"remaining"`
	ResetsAt      time.Time `json:"resets_at"`
}

// GetMySpendLimits (GET /v1/me/spend-limits?currency=) returns the spend windows
// the AUTHENTICATED invoker is enforced against, with live used/reserved/
// remaining and the window's real reset boundary.
//
// SELF-SCOPED BY CONSTRUCTION: both coordinates — the payer account and the
// invoker — come from the resolved principal, never from the request. There is
// no addressing on this route, and naming another subject is refused rather than
// ignored (a silently-ignored parameter reads to the caller as a successful
// cross-read). The payer's admin view of every delegation it has granted stays
// on the treasury route, gated on customer:spend-delegations:read.
func GetMySpendLimits(r *httprequest.Request) {
	if addressed := addressedSpendScope(r); addressed != "" {
		r.ErrorJSON(http.StatusBadRequest, "spend_scope_not_addressable: "+addressed+
			" is not accepted here; /v1/me/spend-limits answers only for the authenticated invoker")
		return
	}

	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	principal, ok := middleware.PrincipalFromRequest(r)
	if !ok {
		r.ErrorJSON(http.StatusUnauthorized, "bearer principal required")
		return
	}
	// A payer's own credential is its own invoker; a delegated credential carries
	// the host-owned invoker it spends under.
	invoker := strings.TrimSpace(principal.Invoker)
	if invoker == "" {
		invoker = strings.TrimSpace(principal.Subject)
	}
	if invoker == "" {
		r.ErrorJSON(http.StatusUnauthorized, "invoker could not be resolved from the credential")
		return
	}

	currency, ok := serviceRequiredCurrency(r, r.Request.URL.Query().Get("currency"))
	if !ok {
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	windows, err := svc.InvokerSpendWindows(r.Request.Context(), payer, billingservice.InvokerSpendWindowsInput{
		Invoker:  invoker,
		Currency: currency,
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "spend window lookup failed")
		return
	}

	out := make([]selfSpendWindow, 0, len(windows))
	for _, w := range windows {
		out = append(out, selfSpendWindow{
			Scope: w.Scope, Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit,
			Currency: w.Currency, Used: w.Used, Reserved: w.Reserved, Remaining: w.Remaining,
			ResetsAt: w.ResetsAt,
		})
	}
	r.SuccessJSON(selfSpendLimitsDocument{Currency: currency, Invoker: invoker, Windows: out})
}

// addressedSpendScope names the first cross-subject addressing parameter present
// on the request, or "" when the caller asked only about itself.
func addressedSpendScope(r *httprequest.Request) string {
	query := r.Request.URL.Query()
	for _, key := range []string{"invoker", "customer_id", "scope_key", "subject"} {
		if query.Has(key) {
			return key
		}
	}
	return ""
}
