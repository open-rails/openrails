package router

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/request"
)

// Entry is an actual registration emitted by the neutral route functions.
type Entry struct {
	Method  string
	Path    string
	Handler http.Handler
	Browser bool
}

// Table records the same registrations that a ServeMux accepts.
type Table struct{ Entries []Entry }

func (t *Table) Handle(pattern string, h http.Handler) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		panic("route table requires a method and path")
	}
	t.Entries = append(t.Entries, Entry{Method: method, Path: path, Handler: h})
}
func (t *Table) HandleFunc(pattern string, h http.HandlerFunc) { t.Handle(pattern, h) }

// Handler serves the table. A path no route serves answers JSON
// route_not_found; a path served for other methods answers method_not_allowed
// with its Allow header.
func (t *Table) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range t.Entries {
		mux.Handle(r.Method+" "+r.Path, r.Handler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &statusProbe{header: http.Header{}}
		mux.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			request.NewHTTP(w, r, nil).AbortCode(billing.CodeMethodNotAllowed, "")
			return
		}
		request.NewHTTP(w, r, nil).AbortCode(billing.CodeRouteNotFound, "")
	})
}

// statusProbe records the status and headers ServeMux answers an unmatched
// request with.
type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header         { return p.header }
func (p *statusProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *statusProbe) WriteHeader(status int)      { p.status = status }

// Wrap retains the global security chain on each native registration. Explicit
// browser OPTIONS make the HTTP contract independent of framework.
func (t *Table) Wrap(wrap func(Entry) http.Handler) {
	originals := append([]Entry(nil), t.Entries...)
	seen := make(map[string]bool)
	for _, r := range originals {
		seen[r.Method+" "+r.Path] = true
	}
	for i, r := range originals {
		t.Entries[i].Handler = wrap(r)
	}
	for i, r := range originals {
		if r.Browser && !seen[http.MethodOptions+" "+r.Path] {
			options := t.Entries[i]
			options.Method = http.MethodOptions
			t.Entries = append(t.Entries, options)
			seen[http.MethodOptions+" "+r.Path] = true
		}
	}
}
