package nmi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestParseGatewayTestModeStrict(t *testing.T) {
	cases := []struct {
		name, body string
		result     TestModeProbeResult
		wantErr    bool
	}{
		{"true", `<nm_response><test_mode_enabled>true</test_mode_enabled></nm_response>`, ProbeSimulated, false},
		{"false", `<nm_response><test_mode_enabled>false</test_mode_enabled></nm_response>`, ProbeLive, false},
		{"missing", `<nm_response/>`, ProbeIndeterminate, true},
		{"duplicate", `<nm_response><test_mode_enabled>true</test_mode_enabled><test_mode_enabled>true</test_mode_enabled></nm_response>`, ProbeIndeterminate, true},
		{"malformed", `<nm_response><test_mode_enabled>true`, ProbeIndeterminate, true},
		{"error", `<nm_response><error_response>bad</error_response></nm_response>`, ProbeIndeterminate, true},
		{"nested", `<nm_response><x><test_mode_enabled>true</test_mode_enabled></x></nm_response>`, ProbeIndeterminate, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGatewayTestMode(tc.body)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.result, got)
		})
	}
}

func TestGatewayQualificationUsesReadOnlyQueryAndNoProbeFallback(t *testing.T) {
	var query, mutations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/query" {
			query++
			require.NoError(t, r.ParseForm())
			require.Equal(t, "test_mode_status", r.Form.Get("report_type"))
			_, _ = w.Write([]byte(`<nm_response><test_mode_enabled>true</test_mode_enabled></nm_response>`))
			return
		}
		mutations++
		http.Error(w, "must not mutate", http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := NewAccountClient(testUUID(), testUUID(), "nmi", &config.NMIProviderSettings{SecurityKey: "captured", EndpointDeployment: config.NMIEndpointGateway}, true)
	require.NoError(t, err)
	client.QueryURL = server.URL + "/query"
	client.V5BaseURL = server.URL
	require.NoError(t, CheckTestModeArm(context.Background(), client))
	require.Equal(t, 1, query)
	require.Equal(t, 0, mutations)
}

func testUUID() uuid.UUID { return uuid.New() }
