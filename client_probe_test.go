package openrails

import (
	"context"
	"encoding"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/routes"
)

// recorder answers every request 418 and keeps it.
type recorder struct{ requests []*http.Request }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests = append(r.requests, req)
	return &http.Response{StatusCode: http.StatusTeapot, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// TestClientProbe calls each client route's method once and checks the
// request it sends against the catalog: method, path, query parameters and
// Idempotency-Key.
func TestClientProbe(t *testing.T) {
	rec := &recorder{}
	merchant := billing.MerchantID(uuid.New())
	remote, err := NewRemote("https://probe.test", RemoteOptions{
		Credentials: map[billing.MerchantID]RemoteCredential{merchant: {APIKey: "probe"}},
		HTTPClient:  &http.Client{Transport: rec},
		ReadCache:   ReadCache{ReadCatalog: {}, ReadMeters: {}, ReadPublicConfig: {}, ReadMerchantConfiguration: {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := remote.Merchant(merchant)
	if err != nil {
		t.Fatal(err)
	}
	probed := 0
	for _, r := range routes.Catalog() {
		if !r.ClientMethod() || isStream(r) {
			continue
		}
		method := reflect.ValueOf(client).MethodByName(r.Name)
		if !method.IsValid() {
			t.Fatalf("%s: Client has no method %s", r.Key(), r.Name)
		}
		args, segments, query := probeArgs(t, r, method.Type())
		rec.requests = nil
		method.Call(args)
		if len(rec.requests) != 1 {
			t.Fatalf("%s: %s sent %d requests", r.Key(), r.Name, len(rec.requests))
		}
		req := rec.requests[0]
		want := r.Path
		for _, segment := range segments {
			want = want[:strings.Index(want, "{")] + url.PathEscape(segment) + want[strings.Index(want, "}")+1:]
		}
		if req.Method != r.Method || req.URL.EscapedPath() != want {
			t.Errorf("%s: %s sent %s %s", r.Key(), r.Name, req.Method, req.URL.EscapedPath())
		}
		var sent []string
		for name := range req.URL.Query() {
			sent = append(sent, name)
		}
		slices.Sort(sent)
		if !slices.Equal(sent, query) {
			t.Errorf("%s: %s sent query %v, the catalog declares %v", r.Key(), r.Name, sent, query)
		}
		if got := req.Header.Get("Idempotency-Key"); (got == "probe-key") != r.IdempotencyKey {
			t.Errorf("%s: %s sent Idempotency-Key %q", r.Key(), r.Name, got)
		}
		probed++
	}
	if probed < 100 {
		t.Fatalf("probed %d methods", probed)
	}
}

func isStream(r routes.Route) bool {
	if _, ok := r.Request.(routes.Stream); ok {
		return true
	}
	for _, reply := range r.Responses {
		if _, ok := reply.Body.(routes.Stream); ok {
			return true
		}
	}
	return false
}

// probeArgs is one call of method: its path segments as sent, and the query
// parameter names a query with every field set sends.
func probeArgs(t *testing.T, r routes.Route, method reflect.Type) (args []reflect.Value, segments, query []string) {
	in := 0
	next := func() reflect.Type { in++; return method.In(in - 1) }
	args = append(args, reflect.ValueOf(context.Background()))
	next()
	if r.IdempotencyKey {
		args = append(args, reflect.ValueOf("probe-key"))
		next()
	}
	for range r.Args {
		v := reflect.New(next()).Elem()
		probeValue(t, v)
		args = append(args, v)
		segments = append(segments, segmentOf(v))
	}
	if r.Request != nil {
		v := reflect.New(next()).Elem()
		if v.Kind() == reflect.Pointer {
			v.Set(reflect.New(v.Type().Elem()))
		}
		args = append(args, v)
	}
	if r.Query != nil {
		v := reflect.New(next()).Elem()
		probeFields(t, v)
		args = append(args, v)
		params, _ := r.QueryParams()
		for _, p := range params {
			query = append(query, p.Name)
		}
	}
	if in != method.NumIn() {
		t.Fatalf("%s: %s takes %d arguments; the catalog gives %d", r.Key(), r.Name, method.NumIn(), in)
	}
	return args, segments, query
}

func segmentOf(v reflect.Value) string {
	if s, ok := v.Interface().(interface{ String() string }); ok {
		return s.String()
	}
	return v.String()
}

// probeFields sets every query field of v.
func probeFields(t *testing.T, v reflect.Value) {
	for i := range v.NumField() {
		f := v.Type().Field(i)
		tag, tagged := f.Tag.Lookup("form")
		switch {
		case !f.IsExported():
		case !tagged && f.Anonymous:
			probeFields(t, v.Field(i))
		case tagged && tag != "-":
			probeValue(t, v.Field(i))
		}
	}
}

// probeValue sets v to a value that is not its zero.
func probeValue(t *testing.T, v reflect.Value) {
	switch {
	case v.Kind() == reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		probeValue(t, v.Elem())
	case v.Type() == reflect.TypeFor[time.Time]():
		v.Set(reflect.ValueOf(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)))
	case v.Kind() == reflect.Slice:
		list := reflect.MakeSlice(v.Type(), 1, 1)
		probeValue(t, list.Index(0))
		v.Set(list)
	case v.Kind() == reflect.Array && v.Len() == 16:
		u := uuid.Must(uuid.NewV7())
		reflect.Copy(v, reflect.ValueOf(u[:]))
	case v.Kind() == reflect.String && v.Addr().Type().Implements(reflect.TypeFor[encoding.TextUnmarshaler]()):
		if err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte("product:price:v1")); err != nil {
			t.Fatalf("no probe value for %s: %v", v.Type(), err)
		}
	case v.Kind() == reflect.String:
		v.SetString("probe")
	case v.Kind() == reflect.Bool:
		v.SetBool(true)
	case v.Kind() >= reflect.Int && v.Kind() <= reflect.Int64:
		v.SetInt(7)
	default:
		t.Fatalf("no probe value for %s", v.Type())
	}
}
