package logx

import (
	"log/slog"
	"reflect"
	"strings"
)

const module = "github.com/open-rails/openrails"

var loggerType = reflect.TypeFor[*slog.Logger]()

// Unset is the path of every nil logger reachable from v: each Log or log
// field of an OpenRails struct, through pointers, interfaces, slices, arrays
// and maps of OpenRails' own types.
func Unset(v any) []string {
	w := walker{seen: map[visit]bool{}}
	w.walk(reflect.ValueOf(v), reflect.TypeOf(v).String())
	return w.unset
}

type visit struct {
	ptr uintptr
	typ reflect.Type
}

type walker struct {
	seen  map[visit]bool
	unset []string
}

func (w *walker) walk(v reflect.Value, path string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() || !owned(v.Type().Elem()) {
			return
		}
		key := visit{v.Pointer(), v.Type()}
		if w.seen[key] {
			return
		}
		w.seen[key] = true
		w.walk(v.Elem(), path)
	case reflect.Interface:
		if !v.IsNil() {
			w.walk(v.Elem(), path)
		}
	case reflect.Struct:
		if !owned(v.Type()) {
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			fv := v.Field(i)
			if f.Type == loggerType && (f.Name == "Log" || f.Name == "log") {
				if fv.IsNil() {
					w.unset = append(w.unset, path+"."+f.Name)
				}
				continue
			}
			w.walk(fv, path+"."+f.Name)
		}
	case reflect.Slice, reflect.Array:
		if !owned(v.Type().Elem()) {
			return
		}
		for i := range v.Len() {
			w.walk(v.Index(i), path+"[]")
		}
	case reflect.Map:
		if !owned(v.Type().Elem()) {
			return
		}
		for it := v.MapRange(); it.Next(); {
			w.walk(it.Value(), path+"[]")
		}
	}
}

// owned reports whether values of t can hold an OpenRails struct.
func owned(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Interface:
		return true
	case reflect.Struct:
		return strings.HasPrefix(t.PkgPath(), module)
	}
	return false
}
