package nmi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

type cutoverVaultRef struct {
	ID        string `json:"id"`
	BillingID string `json:"billing_id,omitempty"`
}

// CreatePausedSubscription enrolls an existing vault without an initial sale.
// A missing/ambiguous receipt must never be retried: NMI assigns the ID.
func (c *NMIClient) CreatePausedSubscription(ctx context.Context, planID, vaultID, billingID string, anchor time.Time) (*V5Subscription, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	if planID == "" || vaultID == "" || anchor.IsZero() {
		return nil, errors.New("plan, vault and billing anchor are required")
	}
	body := struct {
		PlanID        string          `json:"plan_id"`
		CustomerVault cutoverVaultRef `json:"customer_vault"`
		Paused        bool            `json:"paused_subscription"`
		StartDate     string          `json:"start_date"`
	}{planID, cutoverVaultRef{ID: vaultID, BillingID: billingID}, true, anchor.UTC().Format("20060102150405")}
	var receipt V5Subscription
	if err := c.sendV5Request(ctx, http.MethodPost, "/subscriptions", body, &receipt); err != nil {
		return nil, err
	}
	if receipt.ID == "" || receipt.Object != "subscription" {
		return nil, ambiguous(errors.New("missing subscription receipt identity"))
	}
	return &receipt, nil
}

// ActivateSubscription sets a future first charge and unpauses an enrollment.
// The caller must have proved source cancellation and must verify this target.
func (c *NMIClient) ActivateSubscription(ctx context.Context, id string, anchor time.Time) error {
	if err := c.checkConfiguration(); err != nil {
		return err
	}
	if id == "" || anchor.IsZero() {
		return errors.New("subscription and billing anchor are required")
	}
	return c.sendV5Request(ctx, http.MethodPut, "/subscriptions/"+url.PathEscape(id), struct {
		Paused    bool   `json:"paused_subscription"`
		StartDate string `json:"start_date"`
	}{false, anchor.UTC().Format("20060102150405")}, nil)
}

// GetCutoverPlan preserves the complete plan schedule for cutover validation.
func (c *NMIClient) GetCutoverPlan(ctx context.Context, id string) (V5Plan, bool, error) {
	var plan V5Plan
	if err := c.checkConfiguration(); err != nil {
		return plan, false, err
	}
	if id == "" {
		return plan, false, errors.New("plan ID required")
	}
	err := c.sendV5Request(ctx, http.MethodGet, "/plans/"+url.PathEscape(id), nil, &plan)
	if errors.Is(err, ErrV5NotFound) {
		return plan, false, nil
	}
	return plan, err == nil, err
}

// GetCutoverSubscription rejects malformed or wrong-object 200 responses before
// treating an inactive tombstone as cancellation evidence. Only 404 is absence
// without a provider object identity.
func (c *NMIClient) GetCutoverSubscription(ctx context.Context, id string) (V5Subscription, bool, error) {
	var sub V5Subscription
	if err := c.checkConfiguration(); err != nil {
		return sub, false, err
	}
	if id == "" {
		return sub, false, errors.New("subscription ID required")
	}
	err := c.sendV5Request(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(id), nil, &sub)
	if errors.Is(err, ErrV5NotFound) {
		return sub, false, nil
	}
	if err != nil {
		return sub, false, err
	}
	if sub.Object != "subscription" || sub.ID != id || (sub.DelayedCondition != "active" && sub.DelayedCondition != "inactive") {
		return sub, false, errors.New("subscription readback identity or lifecycle mismatch")
	}
	return sub, !sub.cancelledAtNMI(), nil
}

// ConfirmCutoverVault qualifies the one-vault-per-card lane. A vault containing
// several billing records is unsupported because subscription GET does not
// expose the selected billing ID for an exact readback.
func (c *NMIClient) ConfirmCutoverVault(ctx context.Context, id, billingID string) error {
	_, err := c.ReadSingleCardVaultBilling(ctx, id, billingID)
	return err
}

// ReadSingleCardVaultBilling proves the exact sole billing entry under this
// account. A nonempty billing identifier can be a normally-created native card.
func (c *NMIClient) ReadSingleCardVaultBilling(ctx context.Context, id, billingID string) (string, error) {
	if err := c.checkConfiguration(); err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("vault ID required")
	}
	var customer V5Customer
	if err := c.sendV5Request(ctx, http.MethodGet, "/customers/"+url.PathEscape(id), nil, &customer); err != nil {
		return "", err
	}
	if customer.Object != "customer" || customer.ID != id || len(customer.Billing) != 1 || customer.Billing[0].ID == "" || (billingID != "" && customer.Billing[0].ID != billingID) {
		return "", errors.New("cutover requires a verified single-card vault")
	}
	return customer.Billing[0].ID, nil
}
