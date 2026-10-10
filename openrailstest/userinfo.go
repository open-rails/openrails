package openrailstest

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/open-rails/helpers/userinfo"

	"github.com/open-rails/openrails"
)

// UserInfo is an in-memory directory for a host's tests: an
// openrails.UserInfo to pass as Deps.UserInfo in place of AuthKit. It is safe
// for concurrent use.
type UserInfo struct {
	mu   sync.Mutex
	held map[string]userinfo.User
}

var _ openrails.UserInfo = (*UserInfo)(nil)

// Put adds or replaces a user, by its ID.
func (d *UserInfo) Put(u userinfo.User) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.held == nil {
		d.held = map[string]userinfo.User{}
	}
	d.held[u.ID] = u
}

// Delete removes a user.
func (d *UserInfo) Delete(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.held, id)
}

// Get returns the users of ids it holds.
func (d *UserInfo) Get(_ context.Context, ids []string) (map[string]userinfo.User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]userinfo.User, len(ids))
	for _, id := range ids {
		if u, ok := d.held[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

// Search returns at most limit users whose email, username or name contains
// query, ignoring case, by id.
func (d *UserInfo) Search(_ context.Context, query string, limit int) ([]userinfo.User, error) {
	query = strings.ToLower(query)
	if query == "" || limit < 1 {
		return nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []userinfo.User
	for _, u := range d.held {
		for _, v := range []string{u.Email, u.Username, u.Name} {
			if strings.Contains(strings.ToLower(v), query) {
				out = append(out, u)
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
