// Package wireform puts a response in OpenRails' wire form before it is
// marshaled: every time in UTC and every list [] rather than null.
package wireform

import (
	"encoding"
	"encoding/json"
	"reflect"
	"time"
)

var (
	timeType          = reflect.TypeFor[time.Time]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// Of is v in wire form. v itself is not modified.
func Of(v any) any {
	out := value(reflect.ValueOf(v))
	if !out.IsValid() {
		return nil
	}
	return out.Interface()
}

// value copies v with its times in UTC and its nil lists empty. A
// non-struct value that marshals itself is left as it is.
func value(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	t := v.Type()
	switch {
	case t == timeType:
		return reflect.ValueOf(v.Interface().(time.Time).UTC())
	case t.Kind() != reflect.Struct && t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface &&
		(t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType)):
		return v
	}
	switch t.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t.Elem())
		out.Elem().Set(value(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t).Elem()
		out.Set(value(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for i := range t.NumField() {
			if t.Field(i).IsExported() {
				out.Field(i).Set(value(v.Field(i)))
			}
		}
		return out
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return v
		}
		out := reflect.MakeSlice(t, v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(value(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(t).Elem()
		for i := range v.Len() {
			out.Index(i).Set(value(v.Index(i)))
		}
		return out
	case reflect.Map:
		// A nil map stays null: an optional object that is absent.
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), value(it.Value()))
		}
		return out
	}
	return v
}
