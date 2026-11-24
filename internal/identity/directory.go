// Package identity is who a merchant's customers are: their contacts, read
// from the host's directory in process (Deps.UserInfo), else from the copy
// OpenRails keeps of what the merchant's directory pushed over SCIM and
// verified access tokens claimed. Which one is decided at construction.
package identity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"

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

// Customer is the contact as the API shows it: an empty field is absent.
func (c Contact) Customer() *billing.CustomerContact {
	text := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	return &billing.CustomerContact{Email: text(c.Email), Name: text(c.Name), Username: text(c.Username), Active: c.Active, SyncedAt: c.SyncedAt}
}

// Directory reads the merchant's customers' contacts. A customer it holds no
// contact for is absent, never an error.
type Directory interface {
	Contacts(ctx context.Context, merchantID billing.MerchantID, ids []uuid.UUID) (map[uuid.UUID]Contact, error)
	// Search returns at most limit contacts whose email, username or name
	// contains query, ignoring case: people the directory holds, customers
	// of the merchant or not.
	Search(ctx context.Context, merchantID billing.MerchantID, query string, limit int) ([]Contact, error)
	// SearchCustomers is Search kept to the merchant's customers: at most
	// limit, none missed for people who are not its customers, those whose
	// username or email is query first, then the newest. A search it cannot
	// answer in full within its bounds is ErrSearchTooBroad.
	SearchCustomers(ctx context.Context, merchantID billing.MerchantID, query string, limit int) ([]Contact, error)
}

// ErrSearchTooBroad is a customer search matching more people than one
// search reads, of a merchant with more customers than one search reads.
var ErrSearchTooBroad = errors.New("identity: the search matches too many people to search in full")

// Bounds of one customer search: the merchant's customers it reads itself,
// and the matches of a directory it reads in full. It asks for one more:
// AuthKit answers at most 1,000, so 999 is the most it can show complete.
const (
	customerScan = 5000
	searchWindow = 999
)

// Live asks the host's directory on every read. A customer is its issuer's
// subject (OIDC Core §5.7), so each is asked of its issuer's directory: the
// host's own users of Lookup, and a trusted issuer's of the lookup an
// IssuerLookup gives for it. A customer no directory answers for is absent.
type Live struct {
	Lookup userinfo.Lookup
	// DB holds each customer's issuer.
	DB *db.DB
	// rows reads the merchant's customers; nil reads DB.
	rows customerRows
}

// customerRow is one of the merchant's customers as a live directory reads it.
type customerRow struct {
	id        uuid.UUID
	issuer    string
	createdAt time.Time
}

// customerRows is a live directory's read of the merchant's own customers.
type customerRows interface {
	countUpTo(ctx context.Context, mid billing.MerchantID, n int) (int, error)
	// newest is a page of customers, newest first, after the row given.
	newest(ctx context.Context, mid billing.MerchantID, after *customerRow, limit int) ([]customerRow, error)
	byIDs(ctx context.Context, mid billing.MerchantID, ids []uuid.UUID) ([]customerRow, error)
}

func (l Live) customers() customerRows {
	if l.rows != nil {
		return l.rows
	}
	return dbCustomers{l.DB}
}

type dbCustomers struct{ db *db.DB }

func (d dbCustomers) countUpTo(ctx context.Context, mid billing.MerchantID, n int) (int, error) {
	count, err := d.db.Gen(ctx).CountCustomersUpTo(ctx, gen.CountCustomersUpToParams{MerchantID: mid.UUID(), RowLimit: int32(n)}) // #nosec G115 -- a bound of this package
	return int(count), err
}

func (d dbCustomers) newest(ctx context.Context, mid billing.MerchantID, after *customerRow, limit int) ([]customerRow, error) {
	params := gen.ListCustomersParams{MerchantID: mid.UUID(), RowLimit: int32(limit)} // #nosec G115 -- a bound of this package
	if after != nil {
		params.AfterAt, params.AfterID = &after.createdAt, &after.id
	}
	rows, err := d.db.Gen(ctx).ListCustomers(ctx, params)
	return customerRowsOf(rows), err
}

func (d dbCustomers) byIDs(ctx context.Context, mid billing.MerchantID, ids []uuid.UUID) ([]customerRow, error) {
	rows, err := d.db.Gen(ctx).ListCustomersByIDs(ctx, gen.ListCustomersByIDsParams{MerchantID: mid.UUID(), Ids: ids})
	return customerRowsOf(rows), err
}

func customerRowsOf(rows []gen.BillingCustomer) []customerRow {
	out := make([]customerRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, customerRow{id: row.ID, issuer: deref(row.Issuer), createdAt: row.CreatedAt})
	}
	return out
}

// IssuerLookup is a directory with a lookup per merchant and issuer: a
// server's merchants each trust their own issuers.
type IssuerLookup interface {
	// ForIssuer is merchant mid's users of issuer; "" is the host's own.
	ForIssuer(mid billing.MerchantID, issuer string) userinfo.Lookup
	// Issuers are the issuers merchant mid trusts besides the host.
	Issuers(ctx context.Context, mid billing.MerchantID) ([]string, error)
}

// directory is mid's lookup for issuer, nil for none.
func (l Live) directory(mid billing.MerchantID, issuer string) userinfo.Lookup {
	if m, ok := l.Lookup.(IssuerLookup); ok {
		return m.ForIssuer(mid, issuer)
	}
	if issuer == "" {
		return l.Lookup
	}
	return nil
}

// issuers is the issuer of each of mid's customers in ids: "" for the host's
// own users and for ids that are no customer yet.
func (l Live) issuers(ctx context.Context, mid billing.MerchantID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := l.customers().byIDs(ctx, mid, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rows))
	for _, row := range rows {
		out[row.id] = row.issuer
	}
	return out, nil
}

// liveBatch bounds one lookup the host answers.
const liveBatch = 500

func (l Live) Contacts(ctx context.Context, mid billing.MerchantID, ids []uuid.UUID) (map[uuid.UUID]Contact, error) {
	out := make(map[uuid.UUID]Contact, len(ids))
	for start := 0; start < len(ids); start += liveBatch {
		chunk := ids[start:min(start+liveBatch, len(ids))]
		issuers, err := l.issuers(ctx, mid, chunk)
		if err != nil {
			return nil, err
		}
		asked := map[string][]string{}
		for _, id := range chunk {
			asked[issuers[id]] = append(asked[issuers[id]], id.String())
		}
		for issuer, subjects := range asked {
			directory := l.directory(mid, issuer)
			if directory == nil {
				continue
			}
			found, err := directory.Get(ctx, subjects)
			if err != nil {
				return nil, err
			}
			for _, u := range found {
				if contact, ok := liveContact(u); ok && issuers[contact.CustomerID] == issuer {
					out[contact.CustomerID] = contact
				}
			}
		}
	}
	return out, nil
}

func (l Live) Search(ctx context.Context, mid billing.MerchantID, query string, limit int) ([]Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" || limit < 1 {
		return nil, nil
	}
	searched := []string{""}
	if m, ok := l.Lookup.(IssuerLookup); ok {
		more, err := m.Issuers(ctx, mid)
		if err != nil {
			return nil, err
		}
		searched = append(searched, more...)
	}
	out := make([]Contact, 0, limit)
	for _, issuer := range searched {
		directory := l.directory(mid, issuer)
		if directory == nil || len(out) >= limit {
			continue
		}
		found, err := directory.Search(ctx, query, limit-len(out))
		if err != nil {
			return nil, err
		}
		var contacts []Contact
		var ids []uuid.UUID
		for _, u := range found {
			if contact, ok := liveContact(u); ok {
				contacts, ids = append(contacts, contact), append(ids, contact.CustomerID)
			}
		}
		issuers, err := l.issuers(ctx, mid, ids)
		if err != nil {
			return nil, err
		}
		for _, contact := range contacts {
			if issuers[contact.CustomerID] == issuer {
				out = append(out, contact)
			}
		}
	}
	return out, nil
}

// SearchCustomers reads the merchant's own customers' contacts when it has
// few enough, and matches them here; else it filters each directory's
// matches to its customers. A directory never knows whose customer a person
// is, so its first limit matches can all be someone else's.
func (l Live) SearchCustomers(ctx context.Context, mid billing.MerchantID, query string, limit int) ([]Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" || limit < 1 {
		return nil, nil
	}
	customers, err := l.customers().countUpTo(ctx, mid, customerScan+1)
	if err != nil {
		return nil, err
	}
	if customers <= customerScan {
		return l.scanCustomers(ctx, mid, query, limit)
	}
	return l.searchDirectories(ctx, mid, query, limit)
}

// scanCustomers matches every customer's contact, newest first, a page at a
// time.
func (l Live) scanCustomers(ctx context.Context, mid billing.MerchantID, query string, limit int) ([]Contact, error) {
	var found []Contact
	created := map[uuid.UUID]time.Time{}
	var after *customerRow
	for {
		rows, err := l.customers().newest(ctx, mid, after, liveBatch)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		after = &rows[len(rows)-1]
		ids := make([]uuid.UUID, 0, len(rows))
		for _, row := range rows {
			ids, created[row.id] = append(ids, row.id), row.createdAt
		}
		contacts, err := l.Contacts(ctx, mid, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if contact, ok := contacts[id]; ok && contact.matches(query) {
				found = append(found, contact)
			}
		}
		if len(rows) < liveBatch {
			break
		}
	}
	return ranked(found, created, query, limit), nil
}

// searchDirectories keeps each directory's matches that are the merchant's
// customers of its issuer; a directory with more matches than it reads in
// full may hide one.
func (l Live) searchDirectories(ctx context.Context, mid billing.MerchantID, query string, limit int) ([]Contact, error) {
	searched := []string{""}
	if m, ok := l.Lookup.(IssuerLookup); ok {
		more, err := m.Issuers(ctx, mid)
		if err != nil {
			return nil, err
		}
		searched = append(searched, more...)
	}
	var found []Contact
	created := map[uuid.UUID]time.Time{}
	truncated := false
	for _, issuer := range searched {
		directory := l.directory(mid, issuer)
		if directory == nil {
			continue
		}
		matches, err := directory.Search(ctx, query, searchWindow+1)
		if err != nil {
			return nil, err
		}
		truncated = truncated || len(matches) > searchWindow
		var contacts []Contact
		var ids []uuid.UUID
		for _, u := range matches {
			if contact, ok := liveContact(u); ok {
				contacts, ids = append(contacts, contact), append(ids, contact.CustomerID)
			}
		}
		rows, err := l.customers().byIDs(ctx, mid, ids)
		if err != nil {
			return nil, err
		}
		of := make(map[uuid.UUID]string, len(rows))
		for _, row := range rows {
			of[row.id], created[row.id] = row.issuer, row.createdAt
		}
		for _, contact := range contacts {
			if customerIssuer, ok := of[contact.CustomerID]; ok && customerIssuer == issuer {
				found = append(found, contact)
			}
		}
	}
	if truncated && len(found) < limit {
		return nil, ErrSearchTooBroad
	}
	return ranked(found, created, query, limit), nil
}

// ranked is found with the contacts whose username or email is query first,
// then the newest customers, at most limit.
func ranked(found []Contact, created map[uuid.UUID]time.Time, query string, limit int) []Contact {
	slices.SortStableFunc(found, func(a, b Contact) int {
		if ea, eb := a.exact(query), b.exact(query); ea != eb {
			if ea {
				return -1
			}
			return 1
		}
		return created[b.CustomerID].Compare(created[a.CustomerID])
	})
	return found[:min(len(found), limit)]
}

// matches is the directory's search, applied to a contact it answered.
func (c Contact) matches(query string) bool {
	query = strings.ToLower(query)
	for _, v := range []string{c.Email, c.Username, c.Name} {
		if strings.Contains(strings.ToLower(v), query) {
			return true
		}
	}
	return false
}

// exact: the contact's username or email is query, ignoring case.
func (c Contact) exact(query string) bool {
	return strings.EqualFold(c.Username, query) || strings.EqualFold(c.Email, query)
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

func (k Kept) SearchCustomers(ctx context.Context, merchantID billing.MerchantID, query string, limit int) ([]Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" || limit < 1 {
		return nil, nil
	}
	rows, err := k.DB.Gen(ctx).SearchCustomerContactsOfCustomers(ctx, gen.SearchCustomerContactsOfCustomersParams{
		MerchantID: merchantID.UUID(), Pattern: "%" + likeEscaper.Replace(query) + "%", Query: query, RowLimit: int32(min(limit, maxSearch)), // #nosec G115 -- bounded by maxSearch
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

// ErrUsernameUnknown is a username no contact holds, or more than one does.
var ErrUsernameUnknown = errors.New("identity: no customer holds that username")

// Customers reads a customer's username and email for billing email, and
// resolves a provider-supplied username (the CCBill username bridge), from a
// Directory within the merchant ctx is pinned to.
type Customers struct{ Directory Directory }

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

// holderSearch bounds the customers an exact lookup reads: they come
// ranked, those holding the value first.
const holderSearch = 50

// Holders are the contacts whose field (its username or email) is value,
// ignoring case: the merchant's customers holding it when any does, else
// the people the directory holds among the matches it reads in full; none
// when the merchant's customers cannot all be read. A search's first
// matches can all merely contain value.
func Holders(ctx context.Context, d Directory, mid billing.MerchantID, value string, field func(Contact) string) ([]Contact, error) {
	value = strings.TrimSpace(value)
	if value == "" || d == nil {
		return nil, nil
	}
	exact := func(found []Contact) []Contact {
		var out []Contact
		seen := map[uuid.UUID]bool{}
		for _, c := range found {
			if strings.EqualFold(strings.TrimSpace(field(c)), value) && !seen[c.CustomerID] {
				seen[c.CustomerID] = true
				out = append(out, c)
			}
		}
		return out
	}
	customers, err := d.SearchCustomers(ctx, mid, value, holderSearch)
	if errors.Is(err, ErrSearchTooBroad) {
		// Its customers could not all be read: the holder is unknown, not
		// whoever of the whole directory holds it.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if held := exact(customers); len(held) > 0 {
		return held, nil
	}
	people, err := d.Search(ctx, mid, value, searchWindow+1)
	if err != nil {
		return nil, err
	}
	return exact(people), nil
}

// Username is a contact's username, as Holders reads it.
func Username(c Contact) string { return c.Username }

// Email is a contact's email, as Holders reads it.
func Email(c Contact) string { return c.Email }

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
	held, err := Holders(ctx, c.Directory, mid, username, Username)
	if err != nil {
		return "", err
	}
	if len(held) != 1 {
		return "", ErrUsernameUnknown
	}
	return held[0].CustomerID.String(), nil
}
