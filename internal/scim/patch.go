package scim

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// patchOp is one operation of a PatchOp request (RFC 7644 §3.5.2).
type patchOp struct {
	op    string // add, replace or remove
	path  string // lower-cased; empty when value names the attributes
	value any
}

func parsePatch(a attrs) ([]patchOp, *Error) {
	if raw, ok := a["schemas"]; ok {
		list, _ := raw.([]any)
		found := false
		for _, s := range list {
			if v, _ := s.(string); strings.EqualFold(v, PatchOpSchema) {
				found = true
			}
		}
		if !found {
			return nil, errorf(http.StatusBadRequest, "invalidSyntax", "schemas must name %s", PatchOpSchema)
		}
	}
	list, ok := a["operations"].([]any)
	if !ok || len(list) == 0 {
		return nil, errorf(http.StatusBadRequest, "invalidSyntax", "Operations must list at least one operation")
	}
	ops := make([]patchOp, 0, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, errorf(http.StatusBadRequest, "invalidSyntax", "each operation must be an object")
		}
		o := lower(obj)
		op, _, e := o.str("op")
		if e != nil {
			return nil, e
		}
		path, _, e := o.str("path")
		if e != nil {
			return nil, e
		}
		p := patchOp{op: strings.ToLower(op), path: strings.ToLower(path), value: o["value"]}
		switch p.op {
		case "add", "replace":
			if p.value == nil {
				return nil, errorf(http.StatusBadRequest, "invalidValue", "%s needs a value", p.op)
			}
		case "remove":
			if p.path == "" {
				return nil, errorf(http.StatusBadRequest, "noTarget", "remove needs a path")
			}
		default:
			return nil, errorf(http.StatusBadRequest, "invalidSyntax", "op must be add, replace or remove")
		}
		ops = append(ops, p)
	}
	return ops, nil
}

// emailValuePath is emails[...].value: the filter only picks the email, and
// a customer has one.
var emailValuePath = regexp.MustCompile(`^emails\[[^\]]*\]\.value$`)

// apply applies the operation to c, the User with the given id.
func (p patchOp) apply(c *contact, id uuid.UUID) *Error {
	if p.path == "" {
		obj, ok := p.value.(map[string]any)
		if !ok {
			return errorf(http.StatusBadRequest, "invalidValue", "an operation without a path needs an object value")
		}
		for key, value := range obj {
			if e := (patchOp{op: p.op, path: strings.ToLower(key), value: value}).apply(c, id); e != nil {
				return e
			}
		}
		return nil
	}
	remove := p.op == "remove"
	p.path = strings.TrimPrefix(p.path, strings.ToLower(UserSchema)+":")
	str := func() (string, *Error) {
		s, ok := p.value.(string)
		if !ok {
			return "", badValue("%s must be a string", p.path)
		}
		return s, nil
	}
	switch path := p.path; {
	case path == "username":
		if remove {
			return errorf(http.StatusBadRequest, "mutability", "userName is required")
		}
		v, e := str()
		if e != nil {
			return e
		}
		return setUserName(c, v)
	case path == "displayname" || path == "name.formatted":
		if remove {
			c.display = ""
			return nil
		}
		v, e := str()
		if e != nil {
			return e
		}
		return setDisplay(c, v)
	case path == "name":
		if remove {
			c.display = ""
			return nil
		}
		obj, ok := p.value.(map[string]any)
		if !ok {
			return badValue("name must be an object")
		}
		if display := nameDisplay(lower(obj)); display != "" {
			return setDisplay(c, display)
		}
		return nil
	case path == "name.givenname" || path == "name.familyname":
		// Kept only through displayName or name.formatted.
		return nil
	case path == "emails":
		if remove {
			c.email = ""
			return nil
		}
		value := p.value
		if obj, ok := value.(map[string]any); ok {
			value = []any{obj}
		}
		email, e := primaryEmail(value)
		if e != nil {
			return e
		}
		if email == "" {
			return nil
		}
		return setEmail(c, email)
	case path == "emails.value" || emailValuePath.MatchString(path):
		if remove {
			c.email = ""
			return nil
		}
		v, e := str()
		if e != nil {
			return e
		}
		return setEmail(c, v)
	case path == "active":
		if remove {
			c.active = true
			return nil
		}
		active, e := boolean("active", p.value)
		if e != nil {
			return e
		}
		c.active = active
		return nil
	case path == "externalid":
		if v, ok := p.value.(string); !remove && ok {
			if got, ok := customerID(v); ok && got == id {
				return nil
			}
		}
		return errorf(http.StatusBadRequest, "mutability", "externalId is the User's id and cannot change")
	case path == "schemas" || path == "id" || strings.HasPrefix(path, "meta"):
		return nil
	}
	// An attribute this service does not keep (another schema's, a phone
	// number) changes nothing here.
	return nil
}
