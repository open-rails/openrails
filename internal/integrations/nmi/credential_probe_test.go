package nmi

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
		response  string
		wantError bool
	}{
		{name: "valid credentials", response: `<?xml version="1.0"?><nm_response></nm_response>`},
		{name: "rejected credentials", response: `<?xml version="1.0"?><nm_response><error_response>Invalid security key</error_response></nm_response>`, wantError: true},
		{name: "unexpected xml response", response: `<html></html>`, wantError: true},
		{name: "invalid response", response: `not xml`, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(t, r.ParseForm())
				assert.Equal(t, "transaction", r.Form.Get("report_type"))
				assert.Equal(t, "1", r.Form.Get("result_limit"))
				assert.Equal(t, "security-key", r.Form.Get("security_key"))
				_, _ = w.Write([]byte(tt.response))
			}))
			t.Cleanup(server.Close)

			client, err := NewClient("nmi", &config.NMIProviderSettings{SecurityKey: "security-key"}, true)
			require.NoError(t, err)
			client.QueryURL = server.URL

			err = client.ProbeCredentials(t.Context())
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
