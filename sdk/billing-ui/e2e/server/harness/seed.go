package harness

import (
	"context"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails"
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
}

func seedCatalog(ctx context.Context, c *openrails.Client, chain *solanafake.Node, merchant solanago.PublicKey) (Catalog, error) {
	month := 720
	sub, subPrice, err := product(ctx, c, "e2e-membership", "Membership", &openrails.PriceCreateParams{
		Key: "e2e-membership-monthly", UnitAmount: 9_990_000, Currency: "USD", AccessDurationHours: &month, AutoRenew: true,
	})
	if err != nil {
		return Catalog{}, err
	}
	once, oncePrice, err := product(ctx, c, "e2e-lifetime", "Lifetime pass", &openrails.PriceCreateParams{
		Key: "e2e-lifetime-once", UnitAmount: 19_990_000, Currency: "USD",
	})
	if err != nil {
		return Catalog{}, err
	}
	pass, monthly, err := seedCrypto(ctx, c, chain, merchant)
	if err != nil {
		return Catalog{}, err
	}
	return Catalog{SubscriptionProduct: sub, SubscriptionPrice: subPrice, OneTimeProduct: once, OneTimePrice: oncePrice, CryptoPassPrice: pass, CryptoMonthlyPrice: monthly}, nil
}

// seedCrypto applies Solana-sold prices the way a host's catalog does: a
// one-time pass and a monthly membership on a published on-chain plan.
func seedCrypto(ctx context.Context, c *openrails.Client, chain *solanafake.Node, merchant solanago.PublicKey) (string, string, error) {
	plan, err := chain.Plan(merchant, 1078, solanafake.DevnetDUSDMint, 4_990_000, 720)
	if err != nil {
		return "", "", err
	}
	revision, err := c.Catalog.Revision(ctx)
	if err != nil {
		return "", "", err
	}
	params, err := openrails.ParseCatalogApplicationYAML([]byte(fmt.Sprintf(`schema_version: 1
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
	if _, err := c.Catalog.Apply(ctx, params); err != nil {
		return "", "", err
	}
	pass, err := c.Prices.RetrieveByKey(ctx, "e2e-crypto-pass")
	if err != nil {
		return "", "", err
	}
	monthly, err := c.Prices.RetrieveByKey(ctx, "e2e-crypto-monthly")
	if err != nil {
		return "", "", err
	}
	return pass.ID, monthly.ID, nil
}

func product(ctx context.Context, c *openrails.Client, key, name string, price *openrails.PriceCreateParams) (string, string, error) {
	p, err := c.Products.Create(ctx, &openrails.ProductCreateParams{Key: key, DisplayName: name, EntitlementsSpec: map[string]*int{key: nil}})
	if err != nil {
		return "", "", err
	}
	price.ProductID = p.ID
	pr, err := c.Prices.Create(ctx, price)
	if err != nil {
		return "", "", err
	}
	return p.ID, pr.ID, nil
}

type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	AccessToken string `json:"access_token"`
}

// CreateUser registers a native AuthKit user and mints its access token.
func (r *Runtime) CreateUser(ctx context.Context) (User, error) {
	id := uuid.NewString()[:12]
	client := r.Auth.Client()
	email := "e2e-" + id + "@example.test"
	u, err := client.CreateUser(ctx, email, "u"+id)
	if err != nil {
		return User{}, err
	}
	token, _, err := client.MintAccessToken(ctx, u.ID, nil)
	if err != nil {
		return User{}, err
	}
	return User{ID: u.ID, Email: email, AccessToken: token}, nil
}

type Seeded struct {
	SubscriptionSourceID string                         `json:"subscription_source_id"`
	PaymentMethodRef     string                         `json:"payment_method_ref"`
	TransactionID        string                         `json:"transaction_id"`
	Import               *openrails.BillingImportResult `json:"import"`
}

// SeedBilling lands an active subscription, its settled sale and a saved card
// for userID through OpenRails' declared-billing import.
func (r *Runtime) SeedBilling(ctx context.Context, userID string) (Seeded, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return Seeded{}, fmt.Errorf("user_id: %w", err)
	}
	price, err := openrails.ParsePriceID(r.Catalog.SubscriptionPrice)
	if err != nil {
		return Seeded{}, err
	}
	customer := openrails.CustomerID(uid)
	now := time.Now().UTC().Truncate(time.Second)
	started, paidThrough := now.Add(-24*time.Hour), now.Add(29*24*time.Hour)
	railSub, railCustomer, railMethod, txn := "e2e-sub-"+userID, "e2e-cus-"+userID, "e2e-pm-"+userID, "e2e-txn-"+userID
	result, err := r.Client.ImportBilling(ctx, openrails.DeclaredBilling{
		AsOf:       now,
		DefaultPSP: openrails.PSPRef{Key: PSPKey},
		Customers:  []openrails.DeclaredCustomer{{Customer: customer}},
		PaymentMethods: []openrails.DeclaredPaymentMethod{{
			Customer: customer, Rail: PSPRail, RailCustomerRef: railCustomer, RailMethodRef: railMethod,
			LastFour: "4242", CardType: "visa", ExpiryDate: "1230", CreatedAt: started,
		}},
		Subscriptions: []openrails.DeclaredSubscription{{
			SourceID: railSub, Customer: customer, Price: price, Rail: PSPRail, RailSubscriptionID: railSub,
			StartedAt: started, PaidThrough: &paidThrough,
			PaymentMethod: &openrails.PaymentMethodRef{Rail: PSPRail, RailCustomerRef: railCustomer, RailMethodRef: railMethod},
		}},
		Transactions: []openrails.DeclaredTransaction{{
			RailSubscriptionID: railSub, TransactionID: txn, Type: "sale", Success: true,
			AmountCents: 999, Currency: "USD", OccurredAt: started,
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

// CheckoutOffer is what a host serves its checkout page for one price: the
// plan and OpenRails' advertised options, passed through unchanged.
type CheckoutOffer struct {
	Plan    openrails.HostedCheckoutPlan   `json:"plan"`
	Options []openrails.CheckoutRailOption `json:"options"`
}

func (r *Runtime) CheckoutOffer(ctx context.Context, priceID string) (CheckoutOffer, error) {
	price, err := r.Client.Prices.Retrieve(ctx, priceID)
	if err != nil {
		return CheckoutOffer{}, err
	}
	product, err := r.Client.Products.Retrieve(ctx, price.ProductID)
	if err != nil {
		return CheckoutOffer{}, err
	}
	plan, err := openrails.NewHostedCheckoutPlan(product, price)
	if err != nil {
		return CheckoutOffer{}, err
	}
	options, err := r.Client.ListCheckoutRailOptions(ctx, priceID)
	if err != nil {
		return CheckoutOffer{}, err
	}
	return CheckoutOffer{Plan: plan, Options: options}, nil
}

type CheckoutPay struct {
	CustomerID  string `json:"customer_id"`
	PriceID     string `json:"price_id"`
	Selector    string `json:"selector"`
	PSPID       string `json:"psp_id"`
	TokenSymbol string `json:"token_symbol"`
}

// Pay opens the checkout session for the chosen option, as a host's pay
// endpoint does, and answers billing-ui's PayResult.
func (r *Runtime) Pay(ctx context.Context, in CheckoutPay) (map[string]any, error) {
	if _, err := r.Client.EnsureCustomer(ctx, in.CustomerID); err != nil {
		return nil, err
	}
	session, err := r.Client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: in.CustomerID}, PriceID: in.PriceID, IdempotencyKey: uuid.NewString(),
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: in.Selector, PSPID: in.PSPID, TokenSymbol: in.TokenSymbol, Flow: "transaction_request"},
		SuccessURL:     r.BaseURL + "/done", CancelURL: r.BaseURL + "/cancel",
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"status": session.Status}
	if url, ok := session.RailData["solana_pay_url"].(string); ok && url != "" {
		out["transaction_url"] = url
	}
	return out, nil
}
