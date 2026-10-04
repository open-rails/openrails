package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/authkit/iam"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/solanafake"
)

type Catalog struct {
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
}

func seedCatalog(ctx context.Context, c *openrails.Client, chain *solanafake.Node, merchant solanago.PublicKey) (Catalog, error) {
	month := 720
	sub, subPrice, err := product(ctx, c, "e2e-membership", "Membership", billing.CreatePriceParams{
		Key: "e2e-membership-monthly", UnitAmount: 9_990_000, Currency: "USD", AccessDurationHours: &month, AutoRenew: true,
	})
	if err != nil {
		return Catalog{}, err
	}
	once, oncePrice, err := product(ctx, c, "e2e-lifetime", "Lifetime pass", billing.CreatePriceParams{
		Key: "e2e-lifetime-once", UnitAmount: 19_990_000, Currency: "USD",
	})
	if err != nil {
		return Catalog{}, err
	}
	pass, monthly, err := seedCrypto(ctx, c, chain, merchant)
	if err != nil {
		return Catalog{}, err
	}
	cardOnce, cardMonthly, err := seedCards(ctx, c)
	if err != nil {
		return Catalog{}, err
	}
	return Catalog{SubscriptionProduct: sub, SubscriptionPrice: subPrice, OneTimeProduct: once, OneTimePrice: oncePrice, CryptoPassPrice: pass, CryptoMonthlyPrice: monthly, CardOncePrice: cardOnce, CardMonthlyPrice: cardMonthly}, nil
}

// seedCards applies prices only the armed card PSP sells.
func seedCards(ctx context.Context, c *openrails.Client) (string, string, error) {
	revision, err := c.GetCatalogRevision(ctx)
	if err != nil {
		return "", "", err
	}
	params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
application_id: billing-ui-e2e-cards
expected_revision: %d
products:
- key: e2e-card
  display_name: Card membership
  prices:
  - key: e2e-card-once
    currency: usd
    unit_amount: 2990000
    auto_renew: false
    access_duration_hours: 720
    psps: [%s]
  - key: e2e-card-monthly
    currency: usd
    unit_amount: 4990000
    auto_renew: true
    access_duration_hours: 720
    psps: [%s]
  entitlements_spec:
    e2e-card: null
`, revision.Revision, CardPSPKey, CardPSPKey)))
	if err != nil {
		return "", "", err
	}
	if _, err := c.ApplyCatalog(ctx, params); err != nil {
		return "", "", err
	}
	once, err := c.GetPriceByKey(ctx, "e2e-card-once")
	if err != nil {
		return "", "", err
	}
	monthly, err := c.GetPriceByKey(ctx, "e2e-card-monthly")
	if err != nil {
		return "", "", err
	}
	return once.ID.String(), monthly.ID.String(), nil
}

// seedCrypto applies Solana-sold prices the way a host's catalog does: a
// one-time pass and a monthly membership on a published on-chain plan.
func seedCrypto(ctx context.Context, c *openrails.Client, chain *solanafake.Node, merchant solanago.PublicKey) (string, string, error) {
	plan, err := chain.Plan(merchant, 1078, solanafake.DevnetDUSDMint, 4_990_000, 720)
	if err != nil {
		return "", "", err
	}
	revision, err := c.GetCatalogRevision(ctx)
	if err != nil {
		return "", "", err
	}
	params, err := catalog.ParseApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
application_id: billing-ui-e2e-crypto
expected_revision: %d
products:
- key: e2e-crypto
  display_name: Crypto membership
  prices:
  - key: e2e-crypto-pass
    currency: usd
    unit_amount: 2990000
    auto_renew: false
    access_duration_hours: 720
    psps: [solana]
  - key: e2e-crypto-monthly
    currency: usd
    unit_amount: 4990000
    auto_renew: true
    access_duration_hours: 720
    psps: [solana]
    psp_links:
      solana:
        plan_pda: %s
        plan_id: "1078"
  entitlements_spec:
    e2e-crypto: null
`, revision.Revision, plan)))
	if err != nil {
		return "", "", err
	}
	if _, err := c.ApplyCatalog(ctx, params); err != nil {
		return "", "", err
	}
	pass, err := c.GetPriceByKey(ctx, "e2e-crypto-pass")
	if err != nil {
		return "", "", err
	}
	monthly, err := c.GetPriceByKey(ctx, "e2e-crypto-monthly")
	if err != nil {
		return "", "", err
	}
	return pass.ID.String(), monthly.ID.String(), nil
}

func product(ctx context.Context, c *openrails.Client, key, name string, price billing.CreatePriceParams) (string, string, error) {
	p, err := c.CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: name, EntitlementsSpec: map[string]*int{key: nil}})
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

// CreateUser registers a native AuthKit user and mints its access token.
func (r *Runtime) CreateUser(ctx context.Context) (User, error) {
	id := uuid.NewString()[:12]
	email := "e2e-" + id + "@example.test"
	u, err := r.Auth.CreateUser(ctx, iam.NewUser{Email: email, Username: "u" + id})
	if err != nil {
		return User{}, err
	}
	token, err := r.Auth.MintAccessToken(ctx, u.ID, iam.AccessTokenOptions{})
	if err != nil {
		return User{}, err
	}
	return User{ID: u.ID, Email: email, AccessToken: token.Value}, nil
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
	railSub, railCustomer, railMethod, txn := "e2e-sub-"+userID, "e2e-cus-"+userID, "e2e-pm-"+userID, "e2e-txn-"+userID
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

// CustomerBilling is what the customer holds after checkout.
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
	methods, err := r.Client.ListPaymentMethods(ctx, customer, billing.PageRequest{Limit: 100})
	if err != nil {
		return CustomerBilling{}, err
	}
	return CustomerBilling{Subscriptions: subs.Items, PaymentMethods: methods.Items, Sales: r.NMI.Sales(), Vaults: len(r.NMI.Vaults())}, nil
}

func ptr[T any](v T) *T { return &v }
