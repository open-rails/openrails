package harness

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/open-rails/authkit/iam"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
)

type Catalog struct {
	// MerchantID names the merchant in public catalog reads.
	MerchantID          string `json:"merchant_id"`
	SubscriptionProduct string `json:"subscription_product_id"`
	SubscriptionPrice   string `json:"subscription_price_id"`
	OneTimeProduct      string `json:"one_time_product_id"`
	OneTimePrice        string `json:"one_time_price_id"`
	// Prices sold through the Solana PSP (one-time and on-chain plan).
	CryptoPassPrice    string `json:"crypto_pass_price_id"`
	CryptoMonthlyPrice string `json:"crypto_monthly_price_id"`
	// Prices sold through the armed card PSP.
	CardOncePrice    string `json:"card_once_price_id"`
	CardMonthlyPrice string `json:"card_monthly_price_id"`
	// The card membership's higher tier.
	CardPlusPrice string `json:"card_plus_price_id"`
}

func seedCatalog(ctx context.Context, c *openrails.Client) (Catalog, error) {
	month := 720
	sub, err := c.CreateProduct(ctx, billing.CreateProductParams{Key: "e2e-membership", DisplayName: "Membership", Entitlements: []string{"e2e-membership"}})
	if err != nil {
		return Catalog{}, err
	}
	subPrice, err := c.CreateProviderPrice(ctx, billing.CreateProviderPriceParams{
		ProductID: sub.ID, Key: "e2e-membership-monthly", PSP: PSPKey, PlanID: "e2e-membership-monthly", AccessDurationHours: catalog.Value(month),
	})
	if err != nil {
		return Catalog{}, err
	}
	once, oncePrice, err := product(ctx, c, "e2e-lifetime", "Lifetime pass", billing.CreatePriceParams{
		Key: "e2e-lifetime-once", UnitAmount: 19_990_000, Currency: "USD", SellVia: []string{billing.SellViaCard},
	})
	if err != nil {
		return Catalog{}, err
	}
	pass, monthly, err := seedCrypto(ctx, c)
	if err != nil {
		return Catalog{}, err
	}
	cardOnce, cardMonthly, cardPlus, err := seedCards(ctx, c)
	if err != nil {
		return Catalog{}, err
	}
	return Catalog{SubscriptionProduct: sub.ID.String(), SubscriptionPrice: subPrice.ID.String(), OneTimeProduct: once, OneTimePrice: oncePrice, CryptoPassPrice: pass, CryptoMonthlyPrice: monthly, CardOncePrice: cardOnce, CardMonthlyPrice: cardMonthly, CardPlusPrice: cardPlus}, nil
}

// seedCards applies prices sold by card only: a membership and its higher
// tier.
func seedCards(ctx context.Context, c *openrails.Client) (string, string, string, error) {
	params, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
products:
  e2e-card:
    display_name: Card membership
    tier_group: e2e-card
    tier_rank: 1
    prices:
      e2e-card-once:
        currency: usd
        unit_amount: 2990000
        billing_interval_hours: null
        access_duration_hours: 720
        sell_via: [card]
      e2e-card-monthly:
        currency: usd
        unit_amount: 4990000
        billing_interval_hours: 720
        access_duration_hours: 720
        sell_via: [card]
    entitlements: ["e2e-card"]
  e2e-card-plus:
    display_name: Card plus
    tier_group: e2e-card
    tier_rank: 2
    prices:
      e2e-card-plus-monthly:
        currency: usd
        unit_amount: 9990000
        billing_interval_hours: 720
        access_duration_hours: 720
        sell_via: [card]
    entitlements: ["e2e-card", "e2e-card-plus"]
`))
	if err != nil {
		return "", "", "", err
	}
	if _, err := c.ApplyCatalog(ctx, params, billing.ApplyCatalogParams{}); err != nil {
		return "", "", "", err
	}
	var ids [3]string
	for i, key := range [][2]string{{"e2e-card", "e2e-card-once"}, {"e2e-card", "e2e-card-monthly"}, {"e2e-card-plus", "e2e-card-plus-monthly"}} {
		price, err := currentPrice(ctx, c, key[0], key[1])
		if err != nil {
			return "", "", "", err
		}
		ids[i] = price.ID.String()
	}
	return ids[0], ids[1], ids[2], nil
}

// seedCrypto applies Solana-sold prices the way a host's catalog does: a
// one-time pass and a monthly membership on the plan the apply publishes.
func seedCrypto(ctx context.Context, c *openrails.Client) (string, string, error) {
	params, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
products:
  e2e-crypto:
    display_name: Crypto membership
    prices:
      e2e-crypto-pass:
        currency: usd
        unit_amount: 2990000
        billing_interval_hours: null
        access_duration_hours: 720
        sell_via: [solana]
      e2e-crypto-monthly:
        currency: usd
        unit_amount: 4990000
        billing_interval_hours: 720
        access_duration_hours: 720
        sell_via: [solana]
    entitlements: ["e2e-crypto"]
`))
	if err != nil {
		return "", "", err
	}
	if _, err := c.ApplyCatalog(ctx, params, billing.ApplyCatalogParams{}); err != nil {
		return "", "", err
	}
	pass, err := currentPrice(ctx, c, "e2e-crypto", "e2e-crypto-pass")
	if err != nil {
		return "", "", err
	}
	monthly, err := currentPrice(ctx, c, "e2e-crypto", "e2e-crypto-monthly")
	if err != nil {
		return "", "", err
	}
	return pass.ID.String(), monthly.ID.String(), nil
}

// Products is what the host's page carries: its products on sale named by
// keys, their prices as a buyer sees them. The browser never reads the catalog.
func (r *Runtime) Products(ctx context.Context, keys []string) ([]billing.Product, error) {
	page, err := r.Client.ListProducts(ctx, billing.ProductListParams{Keys: keys, Archived: new(false), ForSale: new(true), PageRequest: billing.PageRequest{Limit: billing.MaxBatchItems}})
	if err != nil {
		return nil, err
	}
	for i := range page.Items {
		page.Items[i].Prices = billing.BuyerPrices(page.Items[i].Prices)
	}
	return page.Items, nil
}

func product(ctx context.Context, c *openrails.Client, key, name string, price billing.CreatePriceParams) (string, string, error) {
	p, err := c.CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: name, Entitlements: []string{key}})
	if err != nil {
		return "", "", err
	}
	price.ProductID = p.ID
	pr, err := c.CreatePrice(ctx, price)
	if err != nil {
		return "", "", err
	}
	return p.ID.String(), pr.ID.String(), nil
}

type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	AccessToken string `json:"access_token"`
}

// CreateUser registers a native AuthKit user and signs them in through
// AuthKit's login route: OpenRails admits a user's token only while its
// sign-in stands, and a minted one has none. Each sign-in comes from its own
// address, so the login limit never counts the suite as one client.
func (r *Runtime) CreateUser(ctx context.Context) (User, error) { return r.createUser(ctx, iam.Role{}) }

func (r *Runtime) CreateStaff(ctx context.Context, name string) (User, error) {
	role, ok := r.StaffRoles[name]
	if !ok {
		return User{}, fmt.Errorf("unknown staff role %q", name)
	}
	return r.createUser(ctx, role)
}

func (r *Runtime) createUser(ctx context.Context, role iam.Role) (User, error) {
	id := uuid.NewString()[:12]
	email, password := "e2e-"+id+"@example.test", "e2e-password-"+uuid.NewString()
	u, err := r.Auth.CreateUser(ctx, iam.NewUser{Email: email, Username: "u" + id, Password: password})
	if err != nil {
		return User{}, err
	}
	if !role.IsZero() {
		if _, err := r.Auth.SetGroupRole(ctx, iam.SystemIdentity(), iam.RootGroup(), iam.UserSubject(u.ID), role); err != nil {
			return User{}, err
		}
	}
	body, err := json.Marshal(map[string]string{"identifier": email, "password": password})
	if err != nil {
		return User{}, err
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, r.Auth.APIBase()+"/password/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:1234", rand.IntN(256), rand.IntN(256), rand.IntN(256))
	w := httptest.NewRecorder()
	r.Auth.Handler().ServeHTTP(w, req)
	var out struct {
		TokenSet struct {
			AccessToken string `json:"access_token"`
		} `json:"token_set"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.TokenSet.AccessToken == "" {
		return User{}, fmt.Errorf("sign in %s: %d %s", email, w.Code, w.Body.String())
	}
	return User{ID: u.ID, Email: email, AccessToken: out.TokenSet.AccessToken}, nil
}

type Seeded struct {
	SubscriptionSourceID string                       `json:"subscription_source_id"`
	PaymentMethodRef     string                       `json:"payment_method_ref"`
	TransactionID        string                       `json:"transaction_id"`
	Import               *billing.BillingImportResult `json:"import"`
}

// SeedBilling lands an active subscription, its settled sale and a saved card
// for userID through OpenRails' declared-billing import.
func (r *Runtime) SeedBilling(ctx context.Context, userID string) (Seeded, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return Seeded{}, fmt.Errorf("user_id: %w", err)
	}
	price, err := billing.ParsePriceID(r.Catalog.SubscriptionPrice)
	if err != nil {
		return Seeded{}, err
	}
	customer := billing.CustomerID(uid)
	now := time.Now().UTC().Truncate(time.Second)
	started, paidThrough := now.Add(-24*time.Hour), now.Add(29*24*time.Hour)
	// NMI transaction ids are digits.
	railSub, railCustomer, railMethod, txn := "e2e-sub-"+userID, "e2e-cus-"+userID, "e2e-pm-"+userID, fmt.Sprintf("%020d", binary.BigEndian.Uint64(uid[:8]))
	r.NMI.PutVault(nmimock.Vault{ID: railCustomer, BillingID: railMethod, Card: nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1230"}})
	r.NMI.AddSchedule(nmimock.Schedule{ID: railSub, Vault: railCustomer, Plan: "e2e-membership-monthly", Amount: "9.99", Days: 30, Created: started, NextBilling: paidThrough})
	r.NMI.AddSale(nmimock.Sale{TransactionID: txn, Vault: railCustomer, ScheduleID: railSub, Amount: "9.99", At: started})
	result, err := r.Client.ImportBilling(ctx, billing.DeclaredBilling{
		AsOf:       now,
		DefaultPSP: billing.PSPRef{Key: PSPKey},
		Customers:  []billing.DeclaredCustomer{{Customer: customer}},
		PaymentMethods: []billing.DeclaredPaymentMethod{{
			Customer: customer, Rail: PSPRail, RailCustomerRef: railCustomer, RailMethodRef: railMethod,
			Card: &billing.CardDetails{Brand: ptr("visa"), Last4: ptr("4242"), ExpMonth: ptr(12), ExpYear: ptr(2030)}, CreatedAt: started,
		}},
		Subscriptions: []billing.DeclaredSubscription{{
			SourceID: railSub, Customer: customer, Price: price, Rail: PSPRail, RailSubscriptionID: railSub,
			StartedAt: started, PaidThrough: &paidThrough,
			PaymentMethod: &billing.PaymentMethodRef{Rail: PSPRail, RailCustomerRef: railCustomer, RailMethodRef: railMethod},
		}},
		Transactions: []billing.DeclaredTransaction{{
			RailSubscriptionID: railSub, TransactionID: txn, Type: "sale", Success: true,
			Amount: 9_990_000, Currency: "USD", OccurredAt: started,
		}},
	})
	if err != nil {
		return Seeded{}, err
	}
	if len(result.Blocked) > 0 {
		return Seeded{}, fmt.Errorf("import blocked: %v", result.Reasons)
	}
	return Seeded{SubscriptionSourceID: railSub, PaymentMethodRef: railMethod, TransactionID: txn, Import: result}, nil
}

// CustomerBilling is what the customer holds after purchase.
type CustomerBilling struct {
	Subscriptions  []billing.Subscription  `json:"subscriptions"`
	PaymentMethods []billing.PaymentMethod `json:"payment_methods"`
	Sales          []nmimock.Sale          `json:"sales"`
	Vaults         int                     `json:"vaults"`
}

func (r *Runtime) CustomerBilling(ctx context.Context, customerID string) (CustomerBilling, error) {
	customer, err := billing.ParseCustomerID(customerID)
	if err != nil {
		return CustomerBilling{}, err
	}
	subs, err := r.Client.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: customer})
	if err != nil {
		return CustomerBilling{}, err
	}
	methods, err := r.Client.ListPaymentMethods(ctx, customer, billing.PaymentMethodListParams{PageRequest: billing.PageRequest{Limit: 100}})
	if err != nil {
		return CustomerBilling{}, err
	}
	return CustomerBilling{Subscriptions: subs.Items, PaymentMethods: methods.Items, Sales: r.NMI.Sales(), Vaults: len(r.NMI.Vaults())}, nil
}

// EndSubscriptions cancels the customer's subscriptions at once, access
// included, as staff would.
func (r *Runtime) EndSubscriptions(ctx context.Context, customerID string) error {
	customer, err := billing.ParseCustomerID(customerID)
	if err != nil {
		return err
	}
	subs, err := r.Client.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: customer})
	if err != nil {
		return err
	}
	for _, s := range subs.Items {
		if s.Status == billing.SubscriptionCanceled {
			continue
		}
		if _, err := r.Client.CancelSubscription(ctx, s.ID, billing.CancelSubscriptionParams{Reason: "e2e: ended", RevokeAccess: true}); err != nil {
			return err
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

// currentPrice is the live price a product key and price key name.
func currentPrice(ctx context.Context, c *openrails.Client, productKey, key string) (*billing.Price, error) {
	archived := false
	page, err := c.ListPrices(ctx, billing.PriceListParams{ProductKey: productKey, Key: key, Archived: &archived})
	if err != nil {
		return nil, err
	}
	if len(page.Items) == 0 {
		return nil, fmt.Errorf("no live price %s/%s", productKey, key)
	}
	return &page.Items[0], nil
}
