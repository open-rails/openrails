package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// people is a host directory: Search answers matches in the order people
// joined, at most limit and at most cap, as AuthKit answers 1,000.
type people struct {
	users []userinfo.User
	cap   int
}

func (p *people) join(name string) uuid.UUID {
	id := uuid.New()
	p.users = append(p.users, userinfo.User{ID: id.String(), Username: name, Name: name, Email: name + "@example.com"})
	return id
}

func (p *people) Get(_ context.Context, ids []string) (map[string]userinfo.User, error) {
	out := map[string]userinfo.User{}
	for _, u := range p.users {
		if slices.Contains(ids, u.ID) {
			out[u.ID] = u
		}
	}
	return out, nil
}

func (p *people) Search(_ context.Context, query string, limit int) ([]userinfo.User, error) {
	var out []userinfo.User
	for _, u := range p.users {
		if len(out) >= min(limit, p.cap) {
			break
		}
		if strings.Contains(strings.ToLower(u.Username+" "+u.Email), strings.ToLower(query)) {
			out = append(out, u)
		}
	}
	return out, nil
}

// book is the merchant's customers.
type book struct{ rows []customerRow }

func (b *book) add(id uuid.UUID, at time.Time) {
	b.rows = append(b.rows, customerRow{id: id, createdAt: at})
}

func (b *book) countUpTo(_ context.Context, _ billing.MerchantID, n int) (int, error) {
	return min(len(b.rows), n), nil
}

func (b *book) newest(_ context.Context, _ billing.MerchantID, after *customerRow, limit int) ([]customerRow, error) {
	rows := slices.Clone(b.rows)
	order := func(a, c customerRow) int {
		if n := c.createdAt.Compare(a.createdAt); n != 0 {
			return n
		}
		return bytes.Compare(c.id[:], a.id[:])
	}
	slices.SortFunc(rows, order)
	var out []customerRow
	for _, row := range rows {
		if after != nil && order(*after, row) >= 0 {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, row)
	}
	return out, nil
}

func (b *book) byIDs(_ context.Context, _ billing.MerchantID, ids []uuid.UUID) ([]customerRow, error) {
	var out []customerRow
	for _, row := range b.rows {
		if slices.Contains(ids, row.id) {
			out = append(out, row)
		}
	}
	return out, nil
}

var mid = billing.MerchantID(uuid.New())

func names(found []Contact) []string {
	out := make([]string, 0, len(found))
	for _, c := range found {
		out = append(out, c.Username)
	}
	return out
}

func TestSearchCustomersFindsCustomersPastOtherPeople(t *testing.T) {
	dir, customers := &people{cap: 1000}, &book{}
	for i := range 30 {
		dir.join(fmt.Sprintf("match%02d", i))
	}
	start := time.Now()
	customers.add(dir.join("matchold"), start)
	customers.add(dir.join("match"), start.Add(time.Hour))
	customers.add(dir.join("matchnew"), start.Add(2*time.Hour))
	live := Live{Lookup: dir, rows: customers}
	found, err := live.SearchCustomers(t.Context(), mid, "match", 2)
	require.NoError(t, err)
	require.Equal(t, []string{"match", "matchnew"}, names(found), "the exact match, then the newest")
}

func TestSearchCustomersOfALargeMerchantReadsTheDirectory(t *testing.T) {
	dir, customers := &people{cap: 1000}, &book{}
	start := time.Now()
	for i := range customerScan + 1 {
		customers.add(uuid.New(), start.Add(-time.Duration(i)*time.Second))
	}
	for i := range 20 {
		dir.join(fmt.Sprintf("other%02d", i))
	}
	customers.add(dir.join("otherold"), start.Add(time.Hour))
	customers.add(dir.join("other"), start.Add(2*time.Hour))
	customers.add(dir.join("othernew"), start.Add(3*time.Hour))
	live := Live{Lookup: dir, rows: customers}
	found, err := live.SearchCustomers(t.Context(), mid, "other", 10)
	require.NoError(t, err)
	require.Equal(t, []string{"other", "othernew", "otherold"}, names(found))
}

func TestSearchCustomersRefusesWhatItCannotReadInFull(t *testing.T) {
	setup := func(matches int) Live {
		dir, customers := &people{cap: 1000}, &book{}
		start := time.Now()
		for i := range customerScan + 1 {
			customers.add(uuid.New(), start.Add(-time.Duration(i)*time.Second))
		}
		for i := range matches - 1 {
			dir.join(fmt.Sprintf("crowd%04d", i))
		}
		customers.add(dir.join("crowdbuyer"), start.Add(time.Hour))
		return Live{Lookup: dir, rows: customers}
	}
	// 999 matches are read in full: the one customer among them is found.
	found, err := setup(searchWindow).SearchCustomers(t.Context(), mid, "crowd", 10)
	require.NoError(t, err)
	require.Equal(t, []string{"crowdbuyer"}, names(found))
	// 1,000 may hide more, AuthKit answering no more: refused, not partial.
	_, err = setup(searchWindow+1).SearchCustomers(t.Context(), mid, "crowd", 10)
	require.True(t, errors.Is(err, ErrSearchTooBroad), "%v", err)
}

func TestHoldersFindAnExactUsernamePastContainingMatches(t *testing.T) {
	dir := &people{cap: 1000}
	for i := range 60 {
		dir.join(fmt.Sprintf("bob%02d", i))
	}
	bob := dir.join("bob")
	live := Live{Lookup: dir, rows: &book{}}
	held, err := Holders(t.Context(), live, mid, "BOB", Username)
	require.NoError(t, err)
	require.Len(t, held, 1)
	require.Equal(t, bob, held[0].CustomerID)
	id, err := Customers{Directory: live}.GetUserIDByUsername(merchant.WithID(t.Context(), mid), "bob")
	require.NoError(t, err)
	require.Equal(t, bob.String(), id)
	_, err = Customers{Directory: live}.GetUserIDByUsername(merchant.WithID(t.Context(), mid), "bo")
	require.ErrorIs(t, err, ErrUsernameUnknown, "a name nobody holds exactly")
}

func TestHoldersOfALargeMerchantAreUnknownPastTheWindow(t *testing.T) {
	dir, customers := &people{cap: 1000}, &book{}
	start := time.Now()
	for i := range customerScan + 1 {
		customers.add(uuid.New(), start.Add(-time.Duration(i)*time.Second))
	}
	// A host user, no customer, holds the name, among more matches than a
	// search reads: the merchant's customers cannot all be read, so the
	// directory is not searched for it.
	dir.join("crowd")
	for i := range searchWindow + 1 {
		dir.join(fmt.Sprintf("crowd%04d", i))
	}
	held, err := Holders(t.Context(), Live{Lookup: dir, rows: customers}, mid, "crowd", Username)
	require.NoError(t, err)
	require.Empty(t, held)
}
