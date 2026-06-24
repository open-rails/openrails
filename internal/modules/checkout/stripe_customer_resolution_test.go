package checkout

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes -------------------------------------------------------------------

type fakeCustomerStore struct {
	getID     string
	getErr    error
	upserted  []string // "userID|rail|customerID"
	upsertErr error
	getCalls  int
}

func (f *fakeCustomerStore) GetCustomerID(_ context.Context, _, _ string) (string, error) {
	f.getCalls++
	return f.getID, f.getErr
}

func (f *fakeCustomerStore) Upsert(_ context.Context, userID, rail, customerID string) error {
	f.upserted = append(f.upserted, userID+"|"+rail+"|"+customerID)
	return f.upsertErr
}

type fakeStripeClient struct {
	findID      string
	findErr     error
	findCalls   int
	createID    string
	createErr   error
	createCalls int
	listSubs    []subscriptions.StripeSubscriptionSummary
	listErr     error
}

func (f *fakeStripeClient) FindCustomerIDByAppUserID(_ context.Context, _ string) (string, error) {
	f.findCalls++
	return f.findID, f.findErr
}

func (f *fakeStripeClient) CreateCustomer(_ context.Context, _, _ string) (string, error) {
	f.createCalls++
	return f.createID, f.createErr
}

func (f *fakeStripeClient) ListActiveSubscriptionsForCustomer(_ context.Context, _ string) ([]subscriptions.StripeSubscriptionSummary, error) {
	return f.listSubs, f.listErr
}

type fakePriceResolver struct {
	byStripeID map[string]*models.Price
	err        error
}

func (f *fakePriceResolver) GetByStripePriceID(_ context.Context, id string) (*models.Price, error) {
	if f.err != nil {
		return nil, f.err
	}
	p, ok := f.byStripeID[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return p, nil
}

type fakeProductResolver struct {
	byID map[uuid.UUID]*models.Product
}

func (f *fakeProductResolver) GetByID(_ context.Context, id uuid.UUID) (*models.Product, error) {
	p, ok := f.byID[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return p, nil
}

func userPtr(s string) *string { return &s }

// --- resolution ordering -----------------------------------------------------

func TestResolveStripeCustomer_PrefersLocalMapping(t *testing.T) {
	store := &fakeCustomerStore{getID: "cus_local"}
	client := &fakeStripeClient{findID: "cus_search", createID: "cus_new"}
	user := &UserIdentity{ID: "user-1", Email: userPtr("a@b.com")}

	got, err := resolveStripeCustomerWith(context.Background(), store, client, user)
	require.NoError(t, err)
	assert.Equal(t, "cus_local", got)
	// Local hit: must NOT search or create.
	assert.Equal(t, 0, client.findCalls)
	assert.Equal(t, 0, client.createCalls)
	// Mapping is still re-recorded.
	assert.Equal(t, []string{"user-1|stripe|cus_local"}, store.upserted)
}

func TestResolveStripeCustomer_FallsBackToSearch(t *testing.T) {
	store := &fakeCustomerStore{getID: ""}
	client := &fakeStripeClient{findID: "cus_search", createID: "cus_new"}
	user := &UserIdentity{ID: "user-2"}

	got, err := resolveStripeCustomerWith(context.Background(), store, client, user)
	require.NoError(t, err)
	assert.Equal(t, "cus_search", got)
	assert.Equal(t, 1, client.findCalls)
	assert.Equal(t, 0, client.createCalls) // search hit -> no create
	assert.Equal(t, []string{"user-2|stripe|cus_search"}, store.upserted)
}

func TestResolveStripeCustomer_CreatesWhenMissing(t *testing.T) {
	store := &fakeCustomerStore{getID: ""}
	client := &fakeStripeClient{findID: "", createID: "cus_new"}
	user := &UserIdentity{ID: "user-3"}

	got, err := resolveStripeCustomerWith(context.Background(), store, client, user)
	require.NoError(t, err)
	assert.Equal(t, "cus_new", got)
	assert.Equal(t, 1, client.findCalls)
	assert.Equal(t, 1, client.createCalls)
	assert.Equal(t, []string{"user-3|stripe|cus_new"}, store.upserted)
}

func TestResolveStripeCustomer_TreatsErrNoRowsAsEmptyMapping(t *testing.T) {
	store := &fakeCustomerStore{getErr: sql.ErrNoRows}
	client := &fakeStripeClient{findID: "cus_search"}
	user := &UserIdentity{ID: "user-4"}

	got, err := resolveStripeCustomerWith(context.Background(), store, client, user)
	require.NoError(t, err)
	assert.Equal(t, "cus_search", got)
}

func TestResolveStripeCustomer_NoDepsFallsBackToEmpty(t *testing.T) {
	got, err := resolveStripeCustomerWith(context.Background(), nil, nil, &UserIdentity{ID: "user-5"})
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// --- webhook-independent tier-group guard ------------------------------------

func buildTierGroupFixtures(stripePriceID, tierGroup string) (*fakePriceResolver, *fakeProductResolver) {
	productID := uuid.New()
	price := &models.Price{ProductID: productID}
	product := &models.Product{ID: productID, TierGroup: userPtr(tierGroup)}
	return &fakePriceResolver{byStripeID: map[string]*models.Price{stripePriceID: price}},
		&fakeProductResolver{byID: map[uuid.UUID]*models.Product{productID: product}}
}

func TestStripeTierGroupConflict_BlocksSameTierGroup(t *testing.T) {
	store := &fakeCustomerStore{getID: "cus_1"}
	client := &fakeStripeClient{listSubs: []subscriptions.StripeSubscriptionSummary{
		{ID: "sub_1", Status: "active", PriceID: "price_premium"},
	}}
	prices, products := buildTierGroupFixtures("price_premium", "premium")

	blocked, err := stripeTierGroupConflictWith(context.Background(), store, client, prices, products, &UserIdentity{ID: "u"}, "premium")
	require.NoError(t, err)
	assert.True(t, blocked)
}

func TestStripeTierGroupConflict_AllowsDifferentTierGroup(t *testing.T) {
	store := &fakeCustomerStore{getID: "cus_1"}
	client := &fakeStripeClient{listSubs: []subscriptions.StripeSubscriptionSummary{
		{ID: "sub_1", Status: "active", PriceID: "price_other"},
	}}
	prices, products := buildTierGroupFixtures("price_other", "addon")

	blocked, err := stripeTierGroupConflictWith(context.Background(), store, client, prices, products, &UserIdentity{ID: "u"}, "premium")
	require.NoError(t, err)
	assert.False(t, blocked)
}

func TestStripeTierGroupConflict_NoCustomerNoConflict(t *testing.T) {
	store := &fakeCustomerStore{getID: ""}
	client := &fakeStripeClient{findID: ""} // no customer found anywhere
	prices, products := buildTierGroupFixtures("price_premium", "premium")

	blocked, err := stripeTierGroupConflictWith(context.Background(), store, client, prices, products, &UserIdentity{ID: "u"}, "premium")
	require.NoError(t, err)
	assert.False(t, blocked)
	// Must not attempt to list subs without a customer.
}

func TestStripeTierGroupConflict_UnknownPriceSkipped(t *testing.T) {
	store := &fakeCustomerStore{getID: "cus_1"}
	client := &fakeStripeClient{listSubs: []subscriptions.StripeSubscriptionSummary{
		{ID: "sub_1", Status: "active", PriceID: "price_unmapped"},
	}}
	// Resolver knows a different price only -> unmapped price returns ErrNoRows.
	prices, products := buildTierGroupFixtures("price_known", "premium")

	blocked, err := stripeTierGroupConflictWith(context.Background(), store, client, prices, products, &UserIdentity{ID: "u"}, "premium")
	require.NoError(t, err)
	assert.False(t, blocked)
}

func TestStripeTierGroupConflict_ListErrorPropagates(t *testing.T) {
	store := &fakeCustomerStore{getID: "cus_1"}
	client := &fakeStripeClient{listErr: errors.New("stripe down")}
	prices, products := buildTierGroupFixtures("price_premium", "premium")

	_, err := stripeTierGroupConflictWith(context.Background(), store, client, prices, products, &UserIdentity{ID: "u"}, "premium")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list stripe subscriptions")
}
