package wireform

import (
	"encoding"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A query is a struct's `form` fields: the one spelling the Go client sends
// and the server reads. A field is tagged `form:"name"`, `form:"name,comma"`
// (a list as one comma-separated value; a list is otherwise one parameter
// per value) or `form:"-"`; an untagged embedded struct lends its fields. A
// zero value is absent, except under a pointer. A time is RFC 3339 in UTC
// with its fraction (a date alone reads as its midnight UTC); a value that
// marshals itself as text is that text.

// Param is one query parameter of a query type.
type Param struct {
	Name string
	// Type is the field's type with pointers removed; a list's element type.
	Type reflect.Type
	List bool
	// Comma: the list is one comma-separated value.
	Comma bool
}

// FieldError is a parameter whose value does not parse as its field's type.
type FieldError struct {
	Name string
	Err  error
}

func (e *FieldError) Error() string { return e.Name + ": " + e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

var (
	textMarshaler   = reflect.TypeFor[encoding.TextMarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

type queryField struct {
	Param
	index []int
}

var queryFields sync.Map // reflect.Type -> []queryField

// Params lists t's query parameters, sorted by name. t is a struct, or a
// pointer to one.
func Params(t reflect.Type) ([]Param, error) {
	fields, err := fieldsOf(t)
	if err != nil {
		return nil, err
	}
	out := make([]Param, len(fields))
	for i, f := range fields {
		out[i] = f.Param
	}
	return out, nil
}

func fieldsOf(t reflect.Type) ([]queryField, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if cached, ok := queryFields.Load(t); ok {
		return cached.([]queryField), nil
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("wireform: query type %s is not a struct", t)
	}
	var out []queryField
	seen := map[string]bool{}
	var walk func(t reflect.Type, index []int) error
	walk = func(t reflect.Type, index []int) error {
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			at := append(append([]int(nil), index...), i)
			tag, ok := f.Tag.Lookup("form")
			if !ok {
				if f.Anonymous && f.Type.Kind() == reflect.Struct {
					if err := walk(f.Type, at); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("wireform: %s.%s has no form tag", t, f.Name)
			}
			name, opt, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if name == "" || opt != "" && opt != "comma" {
				return fmt.Errorf("wireform: %s.%s: form tag %q", t, f.Name, tag)
			}
			if seen[name] {
				return fmt.Errorf("wireform: %s names %s twice", t, name)
			}
			seen[name] = true
			p := Param{Name: name, Type: f.Type, Comma: opt == "comma"}
			for p.Type.Kind() == reflect.Pointer {
				p.Type = p.Type.Elem()
			}
			if p.Type.Kind() == reflect.Slice && !scalar(p.Type) {
				p.List, p.Type = true, p.Type.Elem()
			}
			if p.Comma && !p.List {
				return fmt.Errorf("wireform: %s.%s: comma on a scalar", t, f.Name)
			}
			if !scalar(p.Type) {
				return fmt.Errorf("wireform: %s.%s: %s is no query value", t, f.Name, p.Type)
			}
			out = append(out, queryField{Param: p, index: at})
		}
		return nil
	}
	if err := walk(t, nil); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	queryFields.Store(t, out)
	return out, nil
}

// scalar reports a type one parameter value holds.
func scalar(t reflect.Type) bool {
	if t == timeType || t.Implements(textMarshaler) && reflect.PointerTo(t).Implements(textUnmarshaler) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// Encode is v's query: its fields' values by parameter name.
func Encode(v any) (url.Values, error) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	fields, err := fieldsOf(rv.Type())
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for _, f := range fields {
		fv, ok := fieldByIndex(rv, f.index)
		if !ok {
			continue
		}
		forced := false
		for fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				break
			}
			fv, forced = fv.Elem(), true
		}
		if fv.Kind() == reflect.Pointer {
			continue
		}
		if !f.List {
			text, present, err := encodeScalar(fv, forced)
			if err != nil {
				return nil, &FieldError{Name: f.Name, Err: err}
			}
			if present {
				q.Set(f.Name, text)
			}
			continue
		}
		var parts []string
		for i := range fv.Len() {
			text, _, err := encodeScalar(fv.Index(i), true)
			if err != nil {
				return nil, &FieldError{Name: f.Name, Err: err}
			}
			parts = append(parts, text)
		}
		switch {
		case len(parts) == 0:
		case f.Comma:
			q.Set(f.Name, strings.Join(parts, ","))
		default:
			q[f.Name] = parts
		}
	}
	return q, nil
}

// fieldByIndex is the field at index, false under a nil embedded pointer.
func fieldByIndex(v reflect.Value, index []int) (reflect.Value, bool) {
	for i, x := range index {
		if i > 0 {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					return reflect.Value{}, false
				}
				v = v.Elem()
			}
		}
		v = v.Field(x)
	}
	return v, true
}

// encodeScalar is one value's text; a zero value is absent unless forced.
func encodeScalar(v reflect.Value, forced bool) (string, bool, error) {
	if !forced && v.IsZero() {
		return "", false, nil
	}
	if v.Type() == timeType {
		return v.Interface().(time.Time).UTC().Format(time.RFC3339Nano), true, nil
	}
	if v.Type().Implements(textMarshaler) {
		raw, err := v.Interface().(encoding.TextMarshaler).MarshalText()
		return string(raw), err == nil, err
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), true, nil
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), true, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), true, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), true, nil
	}
	return "", false, fmt.Errorf("%s is no query value", v.Type())
}

// Decode sets v's fields from q; v is a pointer to a struct. A parameter
// absent or empty leaves its field as it is; one that does not parse is a
// *FieldError. Parameters v does not declare are ignored.
func Decode(q url.Values, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("wireform: decode into %T, not a pointer", v)
	}
	rv = rv.Elem()
	fields, err := fieldsOf(rv.Type())
	if err != nil {
		return err
	}
	for _, f := range fields {
		values := q[f.Name]
		if len(values) == 0 || !f.List && values[0] == "" {
			continue
		}
		fv := settable(rv, f.index)
		if !f.List {
			if err := decodeScalar(fv, values[0]); err != nil {
				return &FieldError{Name: f.Name, Err: err}
			}
			continue
		}
		if f.Comma {
			var parts []string
			for _, value := range values {
				for _, part := range strings.Split(value, ",") {
					parts = append(parts, strings.TrimSpace(part))
				}
			}
			values = parts
		}
		list := reflect.MakeSlice(fv.Type(), len(values), len(values))
		for i, value := range values {
			if err := decodeScalar(list.Index(i), value); err != nil {
				return &FieldError{Name: f.Name, Err: err}
			}
		}
		fv.Set(list)
	}
	return nil
}

// settable is the field at index, allocating nil pointers on the way and
// its own.
func settable(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 {
			v = deref(v)
		}
		v = v.Field(x)
	}
	return deref(v)
}

func deref(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	return v
}

func decodeScalar(v reflect.Value, raw string) error {
	v = deref(v)
	if v.Type() == timeType {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			var dateErr error
			if t, dateErr = time.Parse(time.DateOnly, raw); dateErr != nil {
				return err
			}
		}
		v.Set(reflect.ValueOf(t))
		return nil
	}
	if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte(raw))
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	default:
		return fmt.Errorf("%s is no query value", v.Type())
	}
	return nil
}
