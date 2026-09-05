//go:build integration

package subscriptions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/stretchr/testify/require"
)

type topupEmailDirectory struct{}

func (topupEmailDirectory) Exists(context.Context, string) (bool, error) { return true, nil }
func (topupEmailDirectory) EmailIdentity(context.Context, string) (string, string, bool, error) {
	return "<customer>", "customer@example.test", true, nil
}

func TestAutoTopupDisabledNotificationDeliversThroughExistingEmailQueue(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(ctx, t, dbi.Pool())
	customer := dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Pool(), uuid.NewString())
	store := merchantconfig.NewStore(dbi)
	require.NoError(t, store.Upsert(ctx, models.MerchantConfiguration{Profile: models.MerchantProfileConfiguration{DisplayName: "Shop", FromEmail: "billing@example.test"}}))
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Subject string `json:"subject"`
			Content []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"content"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "Automatic top-ups disabled", body.Subject)
		require.Len(t, body.Content, 2)
		for _, part := range body.Content {
			require.Contains(t, part.Value, "EUR")
			require.Contains(t, part.Value, "explicitly enable")
		}
		sends.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	email, err := NewEmailService(&config.SendGridConfig{APIKey: "test-only"}, store)
	require.NoError(t, err)
	email.client.BaseURL = server.URL
	email.users = topupEmailDirectory{}
	notifications := NewNotificationService(dbi, email)
	row := &models.NotificationQueue{ID: uuid.New(), CustomerID: customer, EventType: models.NotificationAutoTopupDisabled, CreatedAt: time.Now(), Data: map[string]any{"currency": "EUR"}}
	require.NoError(t, notifications.CreateIfAbsent(ctx, row))
	require.NoError(t, notifications.DeliverEmail(ctx, row))
	require.EqualValues(t, 1, sends.Load())
	var emailed *time.Time
	require.NoError(t, dbi.Pool().QueryRow(ctx, "SELECT emailed_at FROM openrails.notification_queue WHERE id=$1", row.ID).Scan(&emailed))
	require.NotNil(t, emailed)
	content := RenderAutoTopupDisabledEmail("Shop", "<customer>", "EUR")
	require.Contains(t, content.HTML, "&lt;customer&gt;")
	require.NotContains(t, content.HTML, "<customer>")
}
