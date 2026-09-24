package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
)

type Catalog struct {
	SubscriptionProduct string `json:"subscription_product_id"`
	SubscriptionPrice   string `json:"subscription_price_id"`
	OneTimeProduct      string `json:"one_time_product_id"`
	OneTimePrice        string `json:"one_time_price_id"`
}

func seedCatalog(ctx context.Context, c *openrails.Client) (Catalog, error) {
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
	return Catalog{SubscriptionProduct: sub, SubscriptionPrice: subPrice, OneTimeProduct: once, OneTimePrice: oncePrice}, nil
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
