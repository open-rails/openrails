package routes

import (
	"encoding"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/http/wireform"
)

// TestQueryRoundTrip sends every catalog query type with each parameter set
// and reads back what was sent.
func TestQueryRoundTrip(t *testing.T) {
	seen := map[reflect.Type]bool{}
	for _, r := range Catalog() {
		if r.Query == nil || seen[reflect.TypeOf(r.Query)] {
			continue
		}
		typ := reflect.TypeOf(r.Query)
		seen[typ] = true
		in := reflect.New(typ)
		fill(t, in.Elem())
		q, err := wireform.Encode(in.Interface())
		if err != nil {
			t.Fatalf("%s: encode %s: %v", r.Key(), typ, err)
		}
		params, _ := r.QueryParams()
		var names []string
		for _, p := range params {
			names = append(names, p.Name)
		}
		var sent []string
		for name := range q {
			sent = append(sent, name)
		}
		slices.Sort(sent)
		if !slices.Equal(sent, names) {
			t.Fatalf("%s: %s sends %v, declares %v", r.Key(), typ, sent, names)
		}
		out := reflect.New(typ)
		if err := wireform.Decode(q, out.Interface()); err != nil {
			t.Fatalf("%s: decode %s: %v", r.Key(), typ, err)
		}
		if !reflect.DeepEqual(out.Interface(), in.Interface()) {
			t.Fatalf("%s: %s reads back\n%+v\nnot\n%+v", r.Key(), typ, out.Elem(), in.Elem())
		}
	}
}

var sampleTime = time.Date(2026, 3, 4, 5, 6, 7, 8, time.UTC)

// fill sets every query field of v to a value that is not its zero.
func fill(t *testing.T, v reflect.Value) {
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		tag, tagged := f.Tag.Lookup("form")
		switch {
		case !tagged && f.Anonymous:
			fill(t, v.Field(i))
		case tagged && tag != "-":
			sample(t, v.Field(i))
		}
	}
}

func sample(t *testing.T, v reflect.Value) {
	switch {
	case v.Kind() == reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		sample(t, v.Elem())
	case v.Type() == reflect.TypeFor[time.Time]():
		v.Set(reflect.ValueOf(sampleTime))
	case v.Kind() == reflect.Slice:
		list := reflect.MakeSlice(v.Type(), 2, 2)
		for i := range 2 {
			sample(t, list.Index(i))
		}
		v.Set(list)
	case v.Kind() == reflect.Array && v.Type().Elem().Kind() == reflect.Uint8 && v.Len() == 16:
		for i := range 16 {
			v.Index(i).SetUint(uint64(i + 1))
		}
		v.Index(6).SetUint(0x70 | 7) // a version 7 UUID
		v.Index(8).SetUint(0x80 | 9)
	case v.Kind() == reflect.String && v.Addr().Type().Implements(reflect.TypeFor[encoding.TextUnmarshaler]()):
		// A spelled identifier: the first candidate it accepts.
		for _, candidate := range []string{"sample", "product:price:v1"} {
			if v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(candidate)) == nil {
				return
			}
		}
		t.Fatalf("no sample for %s", v.Type())
	case v.Kind() == reflect.String:
		v.SetString("sample")
	case v.Kind() == reflect.Bool:
		v.SetBool(true)
	case v.Kind() >= reflect.Int && v.Kind() <= reflect.Int64:
		v.SetInt(7)
	default:
		t.Fatalf("no sample for %s", v.Type())
	}
}
