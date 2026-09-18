package nmi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// CreatePausedSubscription enrolls an existing vault without an initial sale.
// A missing/ambiguous receipt must never be retried: NMI assigns the ID.
func (c *NMIClient) CreatePausedSubscription(ctx context.Context, planID, vaultID string, anchor time.Time) (*V5Subscription, error) {
	if err := c.checkConfiguration(); err != nil {
		return nil, err
	}
	if planID == "" || vaultID == "" || anchor.IsZero() {
		return nil, errors.New("plan, vault and billing anchor are required")
	}
	body := struct {
		PlanID        string             `json:"plan_id"`
		CustomerVault v5CustomerVaultRef `json:"customer_vault"`
		Paused        bool               `json:"paused_subscription"`
		StartDate     string             `json:"start_date"`
	}{planID, v5CustomerVaultRef{ID: vaultID}, true, anchor.UTC().Format("20060102150405")}
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
