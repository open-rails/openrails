package contract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/http/routes"
)

// obj is a JSON object whose keys keep their insertion order, so the
// generated document is stable and reads top-down.
type obj struct {
	keys []string
	vals map[string]any
}

func newObj(kv ...any) *obj {
	o := &obj{vals: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		o.set(kv[i].(string), kv[i+1])
	}
	return o
}

func (o *obj) set(k string, v any) *obj {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		val, err := marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func marshalIndent(v any) ([]byte, error) {
	raw, err := marshal(v)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	return append(b.Bytes(), '\n'), nil
}

// schema is t's JSON Schema.
func (m *model) schema(t reflect.Type) *obj {
	t = elem(t)
	k, _ := classify(t)
	switch k {
	case kindString:
		if values := m.enums.of(t); values != nil {
			return newObj("type", "string", "enum", values)
		}
		return newObj("type", "string")
	case kindInteger:
		return newObj("type", "integer")
	case kindNumber:
		return newObj("type", "number")
	case kindBoolean:
		return newObj("type", "boolean")
	case kindTime:
		return newObj("type", "string", "format", "date-time")
	case kindAny:
		return newObj()
	case kindArray:
		return newObj("type", "array", "items", m.schema(t.Elem()))
	case kindMap:
		value := m.schema(t.Elem())
		if t.Elem().Kind() == reflect.Pointer {
			value = newObj("oneOf", []any{value, newObj("type", "null")})
		}
		return newObj("type", "object", "additionalProperties", value)
	case kindPage:
		return newObj("type", "object",
			"properties", newObj(
				"data", newObj("type", "array", "items", m.schema(pageItem(t))),
				"next_cursor", newObj("type", []string{"string", "null"}),
			),
			"required", []string{"data", "next_cursor"})
	}
	return newObj("$ref", "#/components/schemas/"+m.names[t])
}

func (m *model) memberSchema(f field) *obj {
	s := m.schema(f.t)
	if f.quoted {
		s = newObj("type", "string", "description", "A number as a decimal string.")
	}
	if f.nullable {
		return newObj("oneOf", []any{s, newObj("type", "null")})
	}
	return s
}

func (m *model) objectSchema(o *object) *obj {
	props := newObj()
	var required []string
	for _, f := range o.fields {
		props.set(f.name, m.memberSchema(f))
		if o.output && !f.optional {
			required = append(required, f.name)
		}
	}
	s := newObj("type", "object", "properties", props)
	if o.t == reflect.TypeFor[catalog.ApplyPrice]() {
		var aliases []any
		for _, name := range []string{"access_duration", "billing_interval", "trial_duration"} {
			props.set(name, newObj("oneOf", []any{
				newObj("type", "string", "description", "A positive whole number of hours, days, or weeks; for example, 72 hours or 3 days. Normalized to "+name+"_hours. Calendar months and years are not supported."),
				newObj("type", "null"),
			}))
			props.set(name+"_hours", newObj("oneOf", []any{
				newObj("type", "integer", "minimum", 1, "maximum", catalog.MaxDurationHours),
				newObj("type", "null"),
			}))
			aliases = append(aliases, newObj("not", newObj("required", []string{name, name + "_hours"})))
		}
		props.set("amount", newObj("type", "string", "description", "A non-negative plain decimal followed by its required registered currency. Uses the currency's native precision exactly, without rounding or currency conversion. Cannot be combined with unit_amount or currency.", "examples", []string{"9.99 USD", "1 SOL", "10 USDC"}))
		for _, name := range []string{"unit_amount", "currency"} {
			aliases = append(aliases, newObj("not", newObj("required", []string{"amount", name})))
		}
		s.set("allOf", aliases)
		s.set("description", "Omitted fields preserve existing terms on updates. amount supplies both unit_amount and currency and cannot be null. Null clears a duration. Each readable duration and its numeric hours alias are mutually exclusive.")
	}
	if len(required) > 0 {
		s.set("required", required)
	}
	return s
}

func (m *model) body(v any) *obj {
	t := reflect.TypeOf(v)
	if stream, ok := v.(routes.Stream); ok {
		return newObj(stream.ContentType, newObj("schema", newObj("type", "string")))
	}
	return newObj("application/json", newObj("schema", m.schema(t)))
}

func (m *model) operation(r routes.Route) *obj {
	op := newObj("tags", []string{string(r.Group)})
	var params []any
	for _, p := range pathParams(r.Path) {
		params = append(params, newObj("name", p, "in", "path", "required", true, "schema", newObj("type", "string")))
	}
	for _, q := range r.Query {
		schema := newObj("type", "string")
		switch q.Kind {
		case "integer", "boolean":
			schema = newObj("type", q.Kind)
		case "date-time":
			schema = newObj("type", "string", "format", "date-time")
		case "ids":
			schema = newObj("type", "array", "items", newObj("type", "string"), "minItems", 1, "maxItems", billing.MaxBatchItems)
			params = append(params, newObj("name", q.Name, "in", "query", "style", "form", "explode", false, "schema", schema))
			continue
		case "strings":
			schema = newObj("type", "array", "items", newObj("type", "string"), "maxItems", billing.MaxBatchItems)
			params = append(params, newObj("name", q.Name, "in", "query", "style", "form", "explode", true, "schema", schema))
			continue
		}
		params = append(params, newObj("name", q.Name, "in", "query", "schema", schema))
	}
	if r.IdempotencyKey {
		params = append(params, newObj("name", "Idempotency-Key", "in", "header", "schema", newObj("type", "string")))
	}
	if len(params) > 0 {
		op.set("parameters", params)
	}
	if r.Request != nil {
		op.set("requestBody", newObj("content", m.body(r.Request)))
	}
	responses := newObj()
	byStatus := map[int][]any{}
	var order []int
	for _, reply := range r.Responses {
		if _, seen := byStatus[reply.Status]; !seen {
			order = append(order, reply.Status)
		}
		byStatus[reply.Status] = append(byStatus[reply.Status], reply.Body)
	}
	for _, status := range order {
		resp := newObj("description", http.StatusText(status))
		bodies := byStatus[status]
		switch {
		case len(bodies) == 1 && bodies[0] != nil:
			resp.set("content", m.body(bodies[0]))
		case len(bodies) > 1:
			var alternatives []any
			for _, body := range bodies {
				alternatives = append(alternatives, m.schema(reflect.TypeOf(body)))
			}
			resp.set("content", newObj("application/json", newObj("schema", newObj("oneOf", alternatives))))
		}
		responses.set(strconv.Itoa(status), resp)
	}
	responses.set("default", newObj("$ref", "#/components/responses/Error"))
	op.set("responses", responses)
	switch r.Auth {
	case routes.AuthPublic, routes.AuthSessionID, routes.AuthProvider:
		op.set("security", []any{})
	case routes.AuthCheckoutSession:
		// The capability alone pays with a new card; the session's own
		// customer, authenticated, also sees and pays with saved cards.
		op.set("security", []any{newObj(), newObj("bearer", []string{})})
	default:
		op.set("security", []any{newObj("bearer", []string{})})
	}
	op.set("x-openrails-auth", string(r.Auth))
	if perm := routePermission(r); perm != "" {
		op.set("x-openrails-permission", perm)
	}
	if r.Sensitive {
		op.set("x-openrails-sensitive", true)
	}
	if r.When != routes.Always {
		op.set("x-openrails-mounted-when", string(r.When))
	}
	if r.CatalogUpdate() {
		op.set("x-openrails-catalog-update", true)
	}
	if r.Limit != "" {
		op.set("x-openrails-operation-limit", string(r.Limit))
	}
	if r.Throttle != "" {
		op.set("x-openrails-throttle", string(r.Throttle))
	}
	op.set("x-openrails-errors", r.Errors)
	op.set("x-openrails-error-sets", r.ErrorSets())
	return op
}

func (m *model) openAPI() ([]byte, error) {
	schemas := newObj()
	for _, o := range m.sortedObjects() {
		schemas.set(o.name, m.objectSchema(o))
	}
	schemas.set("ErrorEnvelope", newObj("type", "object",
		"properties", newObj("error", newObj("$ref", "#/components/schemas/Error")),
		"required", []string{"error"}))
	schemas.set("Error", newObj("type", "object",
		"description", "code is stable and listed in x-openrails-error-codes; type and status follow from it; message is diagnostic and not contract.",
		"properties", newObj(
			"type", newObj("type", "string"),
			"code", newObj("type", "string"),
			"message", newObj("type", "string"),
			"request_id", newObj("type", "string"),
			"param", newObj("type", "string"),
			"metadata", newObj("type", "object"),
		),
		"required", []string{"type", "code", "message"}))

	paths := newObj()
	for _, r := range m.routes {
		item, _ := paths.vals[r.Path].(*obj)
		if item == nil {
			item = newObj()
			paths.set(r.Path, item)
		}
		item.set(strings.ToLower(r.Method), m.operation(r))
	}

	sets, seen := newObj(), map[string]bool{}
	for _, r := range m.routes {
		for _, name := range r.ErrorSets() {
			if !seen[name] {
				seen[name] = true
				sets.set(name, routes.ErrorSet(name))
			}
		}
	}
	sort.Strings(sets.keys)
	codes := newObj()
	for _, c := range errorCodes() {
		codes.set(c.Code, newObj("status", c.Status, "type", c.Type, "meaning", c.Meaning))
	}
	doc := newObj(
		"openapi", "3.1.0",
		"info", newObj(
			"title", "OpenRails",
			"version", "v1",
			"description", "OpenRails' HTTP API, from the API root: a standalone server serves it at /, an embedded host beneath its mount (usually /billing). "+
				"Generated from the route catalog (internal/http/routes) by `go run ./scripts/contracts -write`; do not edit.",
		),
		"paths", paths,
		"components", newObj(
			"schemas", schemas,
			"responses", newObj("Error", newObj("description", "A refusal: a code of the operation's x-openrails-errors or of an x-openrails-error-sets set it names.",
				"content", newObj("application/json", newObj("schema", newObj("$ref", "#/components/schemas/ErrorEnvelope"))))),
			"securitySchemes", newObj("bearer", newObj("type", "http", "scheme", "bearer",
				"description", "An API key, a service or delegated token, or a user session token.")),
		),
		"x-openrails-error-sets", sets,
		"x-openrails-error-codes", codes,
	)
	return marshalIndent(doc)
}

// routePermission is what a route's caller must hold: a staff or
// programmatic route's Routes.Permissions field.
func routePermission(r routes.Route) string { return string(r.Needs()) }
