package router

import (
	"net/http"
	"strings"
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
func (t *Table) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range t.Entries {
		mux.Handle(r.Method+" "+r.Path, r.Handler)
	}
	return mux
}

// Wrap retains the global security chain on each native registration. Explicit
// browser OPTIONS and GET HEAD make the HTTP contract independent of framework.
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
		if r.Method == http.MethodGet && !seen[http.MethodHead+" "+r.Path] {
			head := t.Entries[i]
			head.Method = http.MethodHead
			t.Entries = append(t.Entries, head)
			seen[http.MethodHead+" "+r.Path] = true
		}
		if r.Browser && !seen[http.MethodOptions+" "+r.Path] {
			options := t.Entries[i]
			options.Method = http.MethodOptions
			t.Entries = append(t.Entries, options)
			seen[http.MethodOptions+" "+r.Path] = true
		}
	}
}
