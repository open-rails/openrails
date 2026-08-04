package ccbill

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		response  string
		wantError bool
	}{
		{name: "valid credentials", status: http.StatusOK},
		{name: "rejected credentials", status: http.StatusUnauthorized, wantError: true},
		{name: "error payload", status: http.StatusOK, response: "Error: authentication failed", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(t, r.ParseForm())
				assert.Equal(t, "REBILL", r.Form.Get("transactionTypes"))
				assert.Equal(t, "900000", r.Form.Get("clientAccnum"))
				assert.Equal(t, "0000", r.Form.Get("clientSubacc"))
				assert.Equal(t, "user", r.Form.Get("username"))
				assert.Equal(t, "pass", r.Form.Get("password"))
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.response))
			}))
			t.Cleanup(server.Close)

			client := NewDataLinkClient(&config.CCBillConfig{
				ClientAccNum:     "900000",
				ClientSubAcc:     "0000",
				DataLinkUsername: "user",
				DataLinkPassword: "pass",
			})
			client.BaseURL = server.URL
			client.HTTPClient = server.Client()

			err := client.ProbeCredentials(t.Context())
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
