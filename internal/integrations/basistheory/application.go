package basistheory

import (
	"context"
	"errors"
	"net/http"
)

// TenantID reads the tenant of the application authenticated by this client's key.
func (c *Client) TenantID(ctx context.Context) (string, error) {
	var application struct {
		TenantID string `json:"tenant_id"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/applications/key", nil, "", &application); err != nil {
		return "", err
	}
	if application.TenantID == "" {
		return "", errors.New("basistheory: application response has no tenant id")
	}
	return application.TenantID, nil
}
