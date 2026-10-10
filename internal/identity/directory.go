// Package identity is who a merchant's customers are: their contacts, read
// from the host's directory in process (Deps.UserInfo), else from the copy
// OpenRails keeps of what the merchant's directory pushed over SCIM and
// verified access tokens claimed. Which one is decided at construction.
package identity

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/userinfo"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Contact is how to reach a customer. Active and SyncedAt describe the kept
// copy; a live directory has neither.
type Contact struct {
	CustomerID uuid.UUID
	Email      string
	Name       string
	Username   string
	Active     *bool
	SyncedAt   *time.Time
}

// Directory reads the merchant's customers' contacts. A customer it holds no
// contact for is absent, never an error.
type Directory interface {
	Contacts(ctx context.Context, merchantID billing.MerchantID, ids []uuid.UUID) (map[uuid.UUID]Contact, error)
	// Search returns at most limit contacts whose email, username or name
	// contains query, ignoring case.
	Search(ctx context.Context, merchantID billing.MerchantID, query string, limit int) ([]Contact, error)
}

// Live asks the host's directory on every read. It serves every merchant of
// the engine: an embedded host's directory is its merchant's.
type Live struct{ Lookup userinfo.Lookup }

// liveBatch bounds one lookup the host answers.
const liveBatch = 500

func (l Live) Contacts(ctx context.Context, _ billing.MerchantID, ids []uuid.UUID) (map[uuid.UUID]Contact, error) {
	out := make(map[uuid.UUID]Contact, len(ids))
	for start := 0; start < len(ids); start += liveBatch {
		chunk := ids[start:min(start+liveBatch, len(ids))]
		asked := make([]string, len(chunk))
		for i, id := range chunk {
			asked[i] = id.String()
		}
		found, err := l.Lookup.Get(ctx, asked)
		if err != nil {
			return nil, err
		}
		for _, u := range found {
			if contact, ok := liveContact(u); ok {
				out[contact.CustomerID] = contact
			}
		}
	}
	return out, nil
}

func (l Live) Search(ctx context.Context, _ billing.MerchantID, query string, limit int) ([]Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" || limit < 1 {
		return nil, nil
	}
	found, err := l.Lookup.Search(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Contact, 0, len(found))
	for _, u := range found {
		if contact, ok := liveContact(u); ok {
			out = append(out, contact)
		}
	}
	return out, nil
}

// liveContact is a directory user whose id is a customer id: the host's
// subject UUID.
func liveContact(u userinfo.User) (Contact, bool) {
	id, err := uuid.Parse(strings.TrimSpace(u.ID))
	if err != nil {
		return Contact{}, false
	}
	return Contact{CustomerID: id, Email: strings.TrimSpace(u.Email), Name: strings.TrimSpace(u.Name), Username: strings.TrimSpace(u.Username)}, true
}

// Kept reads billing.customer_contacts.
type Kept struct{ DB *db.DB }

func (k Kept) Contacts(ctx context.Context, merchantID billing.MerchantID, ids []uuid.UUID) (map[uuid.UUID]Contact, error) {
	out := map[uuid.UUID]Contact{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := k.DB.Gen(ctx).ListCustomerContacts(ctx, gen.ListCustomerContactsParams{MerchantID: merchantID.UUID(), CustomerIds: ids})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if contact, ok := KeptContact(row); ok {
			out[row.CustomerID] = contact
		}
	}
	return out, nil
}

func (k Kept) Search(ctx context.Context, merchantID billing.MerchantID, query string, limit int) ([]Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" || limit < 1 {
		return nil, nil
	}
	if limit > maxSearch {
		limit = maxSearch
	}
	rows, err := k.DB.Gen(ctx).SearchCustomerContacts(ctx, gen.SearchCustomerContactsParams{
		MerchantID: merchantID.UUID(), Pattern: "%" + likeEscaper.Replace(query) + "%", Query: query, RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Contact, 0, len(rows))
	for _, row := range rows {
		if contact, ok := KeptContact(row); ok {
			out = append(out, contact)
		}
	}
	return out, nil
}

// maxSearch bounds one search of the kept copy.
const maxSearch = 1000

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// KeptContact is a kept row as a contact; an erased one is none.
func KeptContact(row gen.BillingCustomerContact) (Contact, bool) {
	if row.Email == nil && row.DisplayName == nil && row.UserName == nil && row.Active == nil {
		return Contact{}, false
	}
	synced := row.UpdatedAt.UTC()
	return Contact{CustomerID: row.CustomerID, Email: deref(row.Email), Name: deref(row.DisplayName), Username: deref(row.UserName), Active: row.Active, SyncedAt: &synced}, true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Claims are the contact claims of a verified access token: OIDC's email
// (when verified), name, preferred_username and updated_at. An empty one is
// absent and keeps the recorded value.
type Claims struct {
	Email     string
	Name      string
	Username  string
	UpdatedAt *time.Time
}

// RecordClaims keeps a verified token's contact claims in the merchant's
// copy, newest first: a new customer's are recorded at once, a known one's
// only when dated and newer than what is held. Nothing is written when
// nothing changed.
func RecordClaims(ctx context.Context, q *gen.Queries, merchantID billing.MerchantID, customerID uuid.UUID, c Claims) error {
	email, name, username := bounded(c.Email, 320), bounded(c.Name, 256), bounded(c.Username, 256)
	if email == nil && name == nil && username == nil {
		return nil
	}
	var at *time.Time
	if c.UpdatedAt != nil && !c.UpdatedAt.IsZero() {
		t := c.UpdatedAt.UTC()
		at = &t
	}
	return q.RecordContactClaims(ctx, gen.RecordContactClaimsParams{
		MerchantID: merchantID.UUID(), CustomerID: customerID, Email: email, DisplayName: name, UserName: username, ClaimsUpdatedAt: at,
	})
}

// bounded is a claim the copy can hold: trimmed, nil when blank or longer
// than its column allows.
func bounded(s string, max int) *string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > max {
		return nil
	}
	return &s
}

// UserDirectory reads a customer's username and email for billing email.
type UserDirectory interface {
	EmailIdentity(ctx context.Context, userID string) (username, email string, ok bool, err error)
}

// UsernameResolver resolves a provider-supplied username to a customer. It
// is used only by the CCBill username bridge.
type UsernameResolver interface {
	GetUserIDByUsername(ctx context.Context, username string) (string, error)
}

// ErrUsernameUnknown is a username no contact holds, or more than one does.
var ErrUsernameUnknown = errors.New("identity: no customer holds that username")

// Customers answers both from a Directory, within the merchant ctx is pinned
// to.
type Customers struct{ Directory Directory }

var (
	_ UserDirectory    = Customers{}
	_ UsernameResolver = Customers{}
)

// EmailIdentity is the customer's username and email; ok is false without
// an email.
func (c Customers) EmailIdentity(ctx context.Context, userID string) (string, string, bool, error) {
	id, err := uuid.Parse(strings.TrimSpace(userID))
	if err != nil || c.Directory == nil {
		return "", "", false, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", "", false, err
	}
	found, err := c.Directory.Contacts(ctx, mid, []uuid.UUID{id})
	if err != nil {
		return "", "", false, err
	}
	contact, ok := found[id]
	if !ok || contact.Email == "" {
		return "", "", false, nil
	}
	return contact.Username, contact.Email, true, nil
}

// usernameSearch bounds the candidates a username lookup reads.
const usernameSearch = 50

// GetUserIDByUsername is the one customer whose contact holds username
// (case-insensitively).
func (c Customers) GetUserIDByUsername(ctx context.Context, username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" || c.Directory == nil {
		return "", ErrUsernameUnknown
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	found, err := c.Directory.Search(ctx, mid, username, usernameSearch)
	if err != nil {
		return "", err
	}
	var match []uuid.UUID
	for _, contact := range found {
		if strings.EqualFold(contact.Username, username) {
			match = append(match, contact.CustomerID)
		}
	}
	if len(match) != 1 {
		return "", ErrUsernameUnknown
	}
	return match[0].String(), nil
}
