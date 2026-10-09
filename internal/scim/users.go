package scim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// User is the User resource this service provider returns.
type User struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id"`
	ExternalID  string   `json:"externalId"`
	UserName    string   `json:"userName,omitempty"`
	Name        *Name    `json:"name,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	Emails      []Email  `json:"emails,omitempty"`
	Active      bool     `json:"active"`
	Meta        Meta     `json:"meta"`
}

// Name holds the display name as name.formatted.
type Name struct {
	Formatted string `json:"formatted,omitempty"`
}

// Email is the customer's one email, its primary.
type Email struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary"`
}

// Meta is a resource's metadata.
type Meta struct {
	ResourceType string     `json:"resourceType"`
	Created      *time.Time `json:"created,omitempty"`
	LastModified *time.Time `json:"lastModified,omitempty"`
	Location     string     `json:"location"`
}

// contact is what this service keeps of a User.
type contact struct {
	userName string
	display  string
	email    string
	active   bool
}

func contactOf(row gen.BillingCustomerContact) contact {
	c := contact{userName: text(row.UserName), display: text(row.DisplayName), email: text(row.Email), active: true}
	if row.Active != nil {
		c.active = *row.Active
	}
	return c
}

func text(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// userResource is a kept row as a User.
func userResource(row gen.BillingCustomerContact, base string) User {
	c := contactOf(row)
	id := row.CustomerID.String()
	u := User{Schemas: []string{UserSchema}, ID: id, ExternalID: id, UserName: c.userName, DisplayName: c.display, Active: c.active}
	if c.display != "" {
		u.Name = &Name{Formatted: c.display}
	}
	if c.email != "" {
		u.Emails = []Email{{Value: c.email, Primary: true}}
	}
	modified := row.UpdatedAt.UTC()
	u.Meta = Meta{ResourceType: "User", Created: row.ProvisionedAt, LastModified: &modified, Location: base + "/Users/" + id}
	if u.Meta.Created != nil {
		created := u.Meta.Created.UTC()
		u.Meta.Created = &created
	}
	return u
}

// attrs is a JSON object keyed by lower-cased attribute names: SCIM names
// are case-insensitive.
type attrs map[string]any

func lower(m map[string]any) attrs {
	out := make(attrs, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

func decodeObject(raw []byte) (attrs, *Error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, errorf(http.StatusBadRequest, "invalidSyntax", "the body is not a JSON object")
	}
	return lower(m), nil
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, *Error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPayloadSize))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, errorf(http.StatusRequestEntityTooLarge, "tooLarge", "the body exceeds %d bytes", MaxPayloadSize)
	case err != nil:
		return nil, errorf(http.StatusBadRequest, "invalidSyntax", "the body could not be read")
	}
	return raw, nil
}

func (a attrs) str(key string) (string, bool, *Error) {
	v, ok := a[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, isString := v.(string)
	if !isString {
		return "", false, badValue("%s must be a string", key)
	}
	return strings.TrimSpace(s), true, nil
}

// boolean reads a boolean, or one spelled as a string (Entra ID sends "False").
func boolean(key string, v any) (bool, *Error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, badValue("%s must be a boolean", key)
}

// userInput is a User resource's attributes this service reads.
type userInput struct {
	externalID   *string
	contact      contact
	userNameSet  bool
	lastModified *time.Time
}

// parseUser reads a User resource for POST and PUT. Attributes it does not
// keep are ignored; a missing one is cleared (active defaults to true).
func parseUser(a attrs) (userInput, *Error) {
	in := userInput{contact: contact{active: true}}
	if raw, ok := a["schemas"]; ok {
		list, _ := raw.([]any)
		found := false
		for _, s := range list {
			if v, _ := s.(string); strings.EqualFold(v, UserSchema) {
				found = true
			}
		}
		if !found {
			return in, errorf(http.StatusBadRequest, "invalidSyntax", "schemas must name %s", UserSchema)
		}
	}
	if v, ok, e := a.str("externalid"); e != nil {
		return in, e
	} else if ok {
		in.externalID = &v
	}
	userName, ok, e := a.str("username")
	if e != nil {
		return in, e
	}
	if !ok || userName == "" {
		return in, badValue("userName is required")
	}
	in.userNameSet = true
	if e := setUserName(&in.contact, userName); e != nil {
		return in, e
	}
	display, _, e := a.str("displayname")
	if e != nil {
		return in, e
	}
	if display == "" {
		if raw, ok := a["name"]; ok && raw != nil {
			name, isObject := raw.(map[string]any)
			if !isObject {
				return in, badValue("name must be an object")
			}
			display = nameDisplay(lower(name))
		}
	}
	if e := setDisplay(&in.contact, display); e != nil {
		return in, e
	}
	if raw, ok := a["emails"]; ok && raw != nil {
		email, e := primaryEmail(raw)
		if e != nil {
			return in, e
		}
		if e := setEmail(&in.contact, email); e != nil {
			return in, e
		}
	}
	if raw, ok := a["active"]; ok && raw != nil {
		active, e := boolean("active", raw)
		if e != nil {
			return in, e
		}
		in.contact.active = active
	}
	if raw, ok := a["meta"]; ok && raw != nil {
		meta, _ := raw.(map[string]any)
		if v, ok := lower(meta)["lastmodified"].(string); ok && v != "" {
			at, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return in, badValue("meta.lastModified must be an RFC 3339 instant")
			}
			at = at.UTC()
			in.lastModified = &at
		}
	}
	return in, nil
}

// nameDisplay is a name object's display form: formatted, else the given and
// family names.
func nameDisplay(name attrs) string {
	formatted, _, _ := name.str("formatted")
	if formatted != "" {
		return formatted
	}
	given, _, _ := name.str("givenname")
	family, _, _ := name.str("familyname")
	return strings.TrimSpace(given + " " + family)
}

// primaryEmail is the primary email of an emails array, else its first.
func primaryEmail(raw any) (string, *Error) {
	list, ok := raw.([]any)
	if !ok {
		return "", badValue("emails must be an array")
	}
	first := ""
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return "", badValue("each email must be an object")
		}
		email := lower(obj)
		value, _, e := email.str("value")
		if e != nil {
			return "", e
		}
		if value == "" {
			continue
		}
		if primary, ok := email["primary"]; ok {
			if p, e := boolean("emails.primary", primary); e != nil {
				return "", e
			} else if p {
				return value, nil
			}
		}
		if first == "" {
			first = value
		}
	}
	return first, nil
}

func setUserName(c *contact, v string) *Error {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 256 {
		return badValue("userName must be 1 to 256 bytes")
	}
	c.userName = v
	return nil
}

func setDisplay(c *contact, v string) *Error {
	v = strings.TrimSpace(v)
	if len(v) > 256 {
		return badValue("displayName must be at most 256 bytes")
	}
	c.display = v
	return nil
}

func setEmail(c *contact, v string) *Error {
	v = strings.TrimSpace(v)
	if v != "" && (len(v) > 320 || !strings.Contains(v, "@")) {
		return badValue("an email must be an address of at most 320 bytes")
	}
	c.email = v
	return nil
}

// customerID is a User id or externalId: the customer's UUID.
func customerID(v string) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(v))
	return id, err == nil && id != uuid.Nil
}

// newer reports whether a report made at at replaces values the directory
// last changed at stored.
func newer(at time.Time, stored *time.Time) bool {
	return stored == nil || !at.Before(*stored)
}

func latest(at time.Time, stored *time.Time) *time.Time {
	if stored != nil && stored.After(at) {
		return stored
	}
	return &at
}

// write stores c on the row. Every writer goes through it.
func write(ctx context.Context, q *gen.Queries, row gen.BillingCustomerContact, c contact, provisioned, directoryAt *time.Time) (gen.BillingCustomerContact, error) {
	active := c.active
	return q.UpdateCustomerContact(ctx, gen.UpdateCustomerContactParams{
		MerchantID: row.MerchantID, CustomerID: row.CustomerID,
		Email: nonEmpty(c.email), DisplayName: nonEmpty(c.display), UserName: nonEmpty(c.userName), Active: &active,
		ProvisionedAt: provisioned, DirectoryUpdatedAt: directoryAt,
	})
}

// tx runs fn in a merchant transaction and maps its failure to a SCIM error.
func (s *Server) tx(ctx context.Context, fn func(q *gen.Queries) error) *Error {
	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error { return fn(gen.New(tx)) })
	var refused *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &refused):
		return refused
	case db.IsUniqueViolation(err):
		return errorf(http.StatusConflict, "uniqueness", "another User has this userName")
	}
	log.WithContext(ctx).WithError(err).Error("scim: write failed")
	return errInternal
}

// createUser provisions the User externalId names: the customer it is.
func (s *Server) createUser(ctx context.Context, mid billing.MerchantID, in userInput) (gen.BillingCustomerContact, *Error) {
	if in.externalID == nil {
		return gen.BillingCustomerContact{}, badValue("externalId is required: the user's id at the host, a UUID")
	}
	id, ok := customerID(*in.externalID)
	if !ok {
		return gen.BillingCustomerContact{}, badValue("externalId must be the user's id at the host, a UUID")
	}
	now := s.now()
	at := now
	if in.lastModified != nil {
		at = *in.lastModified
	}
	var out gen.BillingCustomerContact
	e := s.tx(ctx, func(q *gen.Queries) error {
		if err := db.EnsureCustomerRowQ(ctx, q, mid.UUID(), id); err != nil {
			return err
		}
		row, err := q.GetCustomerContactForUpdate(ctx, gen.GetCustomerContactForUpdateParams{MerchantID: mid.UUID(), CustomerID: id})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			active := in.contact.active
			out, err = q.InsertCustomerContact(ctx, gen.InsertCustomerContactParams{
				MerchantID: mid.UUID(), CustomerID: id, Email: nonEmpty(in.contact.email), DisplayName: nonEmpty(in.contact.display),
				UserName: nonEmpty(in.contact.userName), Active: &active, ProvisionedAt: &now, DirectoryUpdatedAt: &at,
			})
			return err
		case err != nil:
			return err
		case row.ProvisionedAt != nil:
			return errorf(http.StatusConflict, "uniqueness", "the User %s exists", id)
		}
		// Claims or an erasure came first: the newer report wins.
		c := in.contact
		if !newer(at, row.DirectoryUpdatedAt) {
			c = contactOf(row)
		}
		out, err = write(ctx, q, row, c, &now, latest(at, row.DirectoryUpdatedAt))
		return err
	})
	return out, e
}

// provisioned locks the User id names, which a SCIM client must hold.
func provisioned(ctx context.Context, q *gen.Queries, mid billing.MerchantID, raw string) (gen.BillingCustomerContact, error) {
	id, ok := customerID(raw)
	if !ok {
		return gen.BillingCustomerContact{}, errNotFound
	}
	row, err := q.GetCustomerContactForUpdate(ctx, gen.GetCustomerContactForUpdateParams{MerchantID: mid.UUID(), CustomerID: id})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return row, errNotFound
	case err != nil:
		return row, err
	case row.ProvisionedAt == nil:
		return row, errNotFound
	}
	return row, nil
}

// replaceUser replaces the User's attributes, unless the directory reports
// them older than what is held.
func (s *Server) replaceUser(ctx context.Context, mid billing.MerchantID, id string, in userInput) (gen.BillingCustomerContact, *Error) {
	if in.externalID != nil {
		if got, ok := customerID(*in.externalID); !ok || got.String() != strings.ToLower(strings.TrimSpace(id)) {
			return gen.BillingCustomerContact{}, errorf(http.StatusBadRequest, "mutability", "externalId is the User's id and cannot change")
		}
	}
	at := s.now()
	if in.lastModified != nil {
		at = *in.lastModified
	}
	var out gen.BillingCustomerContact
	e := s.tx(ctx, func(q *gen.Queries) error {
		row, err := provisioned(ctx, q, mid, id)
		if err != nil {
			return err
		}
		out = row
		if !newer(at, row.DirectoryUpdatedAt) || contactOf(row) == in.contact {
			return nil
		}
		out, err = write(ctx, q, row, in.contact, row.ProvisionedAt, latest(at, row.DirectoryUpdatedAt))
		return err
	})
	return out, e
}

// patchUser applies a PatchOp to the User.
func (s *Server) patchUser(ctx context.Context, mid billing.MerchantID, id string, ops []patchOp) (gen.BillingCustomerContact, *Error) {
	now := s.now()
	var out gen.BillingCustomerContact
	e := s.tx(ctx, func(q *gen.Queries) error {
		row, err := provisioned(ctx, q, mid, id)
		if err != nil {
			return err
		}
		out = row
		c := contactOf(row)
		for _, op := range ops {
			if e := op.apply(&c, row.CustomerID); e != nil {
				return e
			}
		}
		if c == contactOf(row) {
			return nil
		}
		out, err = write(ctx, q, row, c, row.ProvisionedAt, latest(now, row.DirectoryUpdatedAt))
		return err
	})
	return out, e
}

// deleteUser erases the User's contact and keeps the customer's billing. The
// row stays, without values, so an older report cannot restore them.
func (s *Server) deleteUser(ctx context.Context, mid billing.MerchantID, id string) *Error {
	now := s.now()
	return s.tx(ctx, func(q *gen.Queries) error {
		row, err := provisioned(ctx, q, mid, id)
		if err != nil {
			return err
		}
		_, err = q.UpdateCustomerContact(ctx, gen.UpdateCustomerContactParams{
			MerchantID: row.MerchantID, CustomerID: row.CustomerID, DirectoryUpdatedAt: latest(now, row.DirectoryUpdatedAt),
		})
		return err
	})
}

func (s *Server) getUser(ctx context.Context, mid billing.MerchantID, raw string) (gen.BillingCustomerContact, *Error) {
	id, ok := customerID(raw)
	if !ok {
		return gen.BillingCustomerContact{}, errNotFound
	}
	row, err := s.DB.Gen(ctx).GetProvisionedContact(ctx, gen.GetProvisionedContactParams{MerchantID: mid.UUID(), CustomerID: id})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return row, errNotFound
	case err != nil:
		log.WithContext(ctx).WithError(err).Error("scim: read failed")
		return row, errInternal
	}
	return row, nil
}

// CreateUser serves POST /Users.
func (s *Server) CreateUser() http.Handler {
	return s.serve(func(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID) {
		status, body := s.create(ctx, w, r, mid)
		if e, ok := body.(*Error); ok {
			writeError(w, e)
			return
		}
		w.Header().Set("Location", body.(User).Meta.Location)
		writeJSON(w, status, body)
	})
}

func (s *Server) create(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID) (int, any) {
	raw, e := readBody(w, r)
	if e != nil {
		return e.Status, e
	}
	a, e := decodeObject(raw)
	if e != nil {
		return e.Status, e
	}
	return s.createFrom(ctx, mid, a, baseURL(r, "Users"))
}

func (s *Server) createFrom(ctx context.Context, mid billing.MerchantID, a attrs, base string) (int, any) {
	in, e := parseUser(a)
	if e != nil {
		return e.Status, e
	}
	row, e := s.createUser(ctx, mid, in)
	if e != nil {
		return e.Status, e
	}
	return http.StatusCreated, userResource(row, base)
}

// GetUser serves GET /Users/{id}.
func (s *Server) GetUser() http.Handler {
	return s.serve(func(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID) {
		row, e := s.getUser(ctx, mid, r.PathValue("id"))
		if e != nil {
			writeError(w, e)
			return
		}
		writeJSON(w, http.StatusOK, userResource(row, baseURL(r, "Users")))
	})
}

// ReplaceUser serves PUT /Users/{id}.
func (s *Server) ReplaceUser() http.Handler {
	return s.serve(func(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID) {
		raw, e := readBody(w, r)
		if e != nil {
			writeError(w, e)
			return
		}
		a, e := decodeObject(raw)
		if e != nil {
			writeError(w, e)
			return
		}
		status, body := s.replaceFrom(ctx, mid, r.PathValue("id"), a, baseURL(r, "Users"))
		writeBody(w, status, body)
	})
}

func (s *Server) replaceFrom(ctx context.Context, mid billing.MerchantID, id string, a attrs, base string) (int, any) {
	in, e := parseUser(a)
	if e != nil {
		return e.Status, e
	}
	row, e := s.replaceUser(ctx, mid, id, in)
	if e != nil {
		return e.Status, e
	}
	return http.StatusOK, userResource(row, base)
}

// PatchUser serves PATCH /Users/{id}.
func (s *Server) PatchUser() http.Handler {
	return s.serve(func(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID) {
		raw, e := readBody(w, r)
		if e != nil {
			writeError(w, e)
			return
		}
		a, e := decodeObject(raw)
		if e != nil {
			writeError(w, e)
			return
		}
		status, body := s.patchFrom(ctx, mid, r.PathValue("id"), a, baseURL(r, "Users"))
		writeBody(w, status, body)
	})
}

func (s *Server) patchFrom(ctx context.Context, mid billing.MerchantID, id string, a attrs, base string) (int, any) {
	ops, e := parsePatch(a)
	if e != nil {
		return e.Status, e
	}
	row, e := s.patchUser(ctx, mid, id, ops)
	if e != nil {
		return e.Status, e
	}
	return http.StatusOK, userResource(row, base)
}

// DeleteUser serves DELETE /Users/{id}.
func (s *Server) DeleteUser() http.Handler {
	return s.serve(func(ctx context.Context, w http.ResponseWriter, r *http.Request, mid billing.MerchantID) {
		if e := s.deleteUser(ctx, mid, r.PathValue("id")); e != nil {
			writeError(w, e)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeBody(w http.ResponseWriter, status int, body any) {
	if e, ok := body.(*Error); ok {
		writeError(w, e)
		return
	}
	writeJSON(w, status, body)
}
