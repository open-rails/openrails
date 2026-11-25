package hostconfig

import (
	"reflect"
	"strings"

	"github.com/open-rails/authkit"
)

// authPath is the auth section key an AUTH_* environment variable names:
// its rest after AUTH_, matched against AuthKit's keys (sign_in_dpop is
// sign_in.dpop). A rest naming no key stays whole, so boot refuses it.
func authPath(rest string) string {
	if p := yamlPath(reflect.TypeOf(authkit.Config{}), rest); p != "" {
		return p
	}
	return rest
}

func yamlPath(t reflect.Type, rest string) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		if rest == name {
			return name
		}
		if sub, ok := strings.CutPrefix(rest, name+"_"); ok {
			if p := yamlPath(t.Field(i).Type, sub); p != "" {
				return name + "." + p
			}
		}
	}
	return ""
}
