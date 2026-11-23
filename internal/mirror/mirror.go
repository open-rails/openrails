// Package mirror checks a conversion between two types that mirror each
// other field by field, such as the public Config and the engine's: fill one
// side, convert, and every field of the other side is set. Tests use it.
package mirror

import (
	"reflect"
	"slices"
	"strings"
)

// depth bounds Fill and Zero on recursive types.
const depth = 8

// Fill sets every exported field reachable from ptr to a non-zero value:
// pointers are allocated, slices and maps get one element, an interface takes
// a string where it can.
func Fill(ptr any) { fill(reflect.ValueOf(ptr).Elem(), depth) }

func fill(v reflect.Value, d int) {
	if d == 0 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), d-1)
			}
		}
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), d-1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), d-1)
		v.Set(s)
	case reflect.Array:
		for i := range v.Len() {
			fill(v.Index(i), d-1)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, d-1)
		fill(e, d-1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Interface:
		if s := reflect.ValueOf("x"); s.Type().AssignableTo(v.Type()) {
			v.Set(s)
		}
	}
}

// Zero names the zero fields of v, descending into the structs, pointers,
// slices and maps of the types packages pkgs declare; another type's value
// only needs to be non-zero.
func Zero(v any, pkgs ...string) []string {
	var out []string
	zero(reflect.ValueOf(v), reflect.TypeOf(v).Name(), pkgs, depth, &out)
	return out
}

func zero(v reflect.Value, path string, pkgs []string, d int, out *[]string) {
	if v.IsZero() {
		*out = append(*out, path)
		return
	}
	t := v.Type()
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if d == 0 || (t.Name() != "" && !slices.Contains(pkgs, t.PkgPath())) {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				zero(v.Field(i), path+"."+f.Name, pkgs, d-1, out)
			}
		}
	case reflect.Pointer:
		zero(v.Elem(), path, pkgs, d-1, out)
	case reflect.Slice:
		zero(v.Index(0), path+"[0]", pkgs, d-1, out)
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			zero(iter.Value(), path+"[]", pkgs, d-1, out)
		}
	}
}

// Missing names the exported fields of a (descending into its struct
// fields, through pointers, slices and maps) that b has no field of the same
// name for.
func Missing(a, b reflect.Type) []string {
	var out []string
	missing(a, b, a.Name(), depth, &out)
	return out
}

func missing(a, b reflect.Type, path string, d int, out *[]string) {
	a, b = elem(a), elem(b)
	if d == 0 || a.Kind() != reflect.Struct || b.Kind() != reflect.Struct || a == b {
		return
	}
	for i := range a.NumField() {
		f := a.Field(i)
		if !f.IsExported() {
			continue
		}
		g, ok := b.FieldByName(f.Name)
		if !ok {
			*out = append(*out, strings.TrimPrefix(path+"."+f.Name, "."))
			continue
		}
		missing(f.Type, g.Type, path+"."+f.Name, d-1, out)
	}
}

func elem(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	return t
}
