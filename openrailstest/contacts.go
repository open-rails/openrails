package openrailstest

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/open-rails/openrails"
)

// Contacts is an in-memory directory for a host's tests: an
// openrails.Contacts to pass as Deps.Contacts in place of AuthKit. It is safe
// for concurrent use.
type Contacts struct {
	mu   sync.Mutex
	held map[string]openrails.Contact
}

var _ openrails.Contacts = (*Contacts)(nil)

// Put adds or replaces a contact, by its ID.
func (d *Contacts) Put(c openrails.Contact) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.held == nil {
		d.held = map[string]openrails.Contact{}
	}
	d.held[c.ID] = c
}

// Delete removes a contact.
func (d *Contacts) Delete(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.held, id)
}

// Contacts returns the contacts of ids it holds.
func (d *Contacts) Contacts(_ context.Context, ids []string) (map[string]openrails.Contact, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]openrails.Contact, len(ids))
	for _, id := range ids {
		if c, ok := d.held[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

// SearchContacts returns at most limit contacts whose email, username or name
// contains query, ignoring case, by id.
func (d *Contacts) SearchContacts(_ context.Context, query string, limit int) ([]openrails.Contact, error) {
	query = strings.ToLower(query)
	if query == "" || limit < 1 {
		return nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []openrails.Contact
	for _, c := range d.held {
		for _, v := range []string{c.Email, c.Username, c.Name} {
			if strings.Contains(strings.ToLower(v), query) {
				out = append(out, c)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
