package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReplayService_LoadWebhookEvents tests loading webhook event files
func TestReplayService_LoadWebhookEvents(t *testing.T) {
	rs := &ReplayService{}

	tests := []struct {
		name        string
		rail        string
		eventFilter string
		expectError bool
		minFiles    int
	}{
		{
			name:        "Load all CCBill events",
			rail:        "ccbill",
			eventFilter: "all",
			expectError: false,
			minFiles:    1,
		},
		{
			name:        "Load all NMI events",
			rail:        "mobius",
			eventFilter: "all",
			expectError: false,
			minFiles:    1,
		},
		{
			name:        "Load specific CCBill event",
			rail:        "ccbill",
			eventFilter: "newsalesuccess.json",
			expectError: false,
			minFiles:    1,
		},
		{
			name:        "Load specific NMI event",
			rail:        "mobius",
			eventFilter: "recurring_subscription_add.json",
			expectError: false,
			minFiles:    1,
		},
		{
			name:        "Invalid rail",
			rail:        "invalid",
			eventFilter: "all",
			expectError: true,
		},
		{
			name:        "Nonexistent event file",
			rail:        "ccbill",
			eventFilter: "nonexistent.json",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eventFiles, err := rs.loadWebhookEvents(tt.rail, tt.eventFilter)

			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, eventFiles)
			} else {
				assert.NoError(t, err)
				assert.GreaterOrEqual(t, len(eventFiles), tt.minFiles)

				// Verify all files exist and are JSON files
				for _, file := range eventFiles {
					assert.True(t, filepath.Ext(file) == ".json", "File should have .json extension: %s", file)
					_, err := os.Stat(file)
					assert.NoError(t, err, "File should exist: %s", file)
				}
			}
		})
	}
}

// TestReplayService_ValidateWebhookPayload tests webhook payload validation
func TestReplayService_ValidateWebhookPayload(t *testing.T) {
	rs := &ReplayService{}

	// Get a real webhook file for testing
	eventFiles, err := rs.loadWebhookEvents("ccbill", "newsalesuccess.json")
	require.NoError(t, err)
	require.NotEmpty(t, eventFiles)

	result, err := rs.validateWebhookPayload(eventFiles[0])
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "newsalesuccess.json", result.EventFile)
	assert.Equal(t, "ccbill", result.Rail)
	assert.Empty(t, result.Error)

	// Test NMI event validation
	nmiFiles, err := rs.loadWebhookEvents("mobius", "recurring_subscription_add.json")
	require.NoError(t, err)
	require.NotEmpty(t, nmiFiles)

	nmiResult, err := rs.validateWebhookPayload(nmiFiles[0])
	require.NoError(t, err)
	assert.True(t, nmiResult.Success)
	assert.Equal(t, "recurring_subscription_add.json", nmiResult.EventFile)
	assert.Equal(t, "mobius", nmiResult.Rail)
	assert.NotEmpty(t, nmiResult.EventType, "NMI events should have event_type extracted")
}

// TestReplayService_DryRun tests dry run functionality
func TestReplayService_DryRun(t *testing.T) {
	rs := &ReplayService{
		DryRun:     true,
		Verbose:    true,
		Concurrent: 1,
	}

	ctx := context.Background()

	// Test CCBill dry run
	successCount, failureCount, err := rs.ReplayCCBillWebhooks(ctx, "all")
	assert.NoError(t, err)
	totalEvents := successCount + failureCount
	assert.Greater(t, totalEvents, 0, "Should have processed some events")
	t.Logf("CCBill validation results: %d success, %d failures", successCount, failureCount)

	// Test NMI dry run
	successCount, failureCount, err = rs.ReplayNMIWebhooks(ctx, "all")
	assert.NoError(t, err)
	totalEvents = successCount + failureCount
	assert.Greater(t, totalEvents, 0, "Should have processed some events")
	t.Logf("NMI validation results: %d success, %d failures", successCount, failureCount)
}

// TestReplayService_WebhookReplay tests actual webhook replay functionality
func TestReplayService_WebhookReplay(t *testing.T) {
	// Create a test HTTP server to receive webhooks
	var receivedRequests []TestWebhookRequest

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener not permitted in this environment: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		request := TestWebhookRequest{
			Method:      r.Method,
			URL:         r.URL.String(),
			Headers:     make(map[string]string),
			Body:        string(body),
			ContentType: r.Header.Get("Content-Type"),
		}

		// Copy headers
		for key, values := range r.Header {
			if len(values) > 0 {
				request.Headers[key] = values[0]
			}
		}

		receivedRequests = append(receivedRequests, request)

		// Return success response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "webhook processed successfully"})
	}))
	server.Listener = ln
	server.Start()
	defer server.Close()

	rs := &ReplayService{
		TargetEndpoint: server.URL,
		Concurrent:     1,
		Delay:          10 * time.Millisecond,
		DryRun:         false,
		Verbose:        true,
	}

	ctx := context.Background()

	// Test replaying a single CCBill event
	successCount, failureCount, err := rs.ReplayCCBillWebhooks(ctx, "newsalesuccess.json")
	assert.NoError(t, err)
	assert.Equal(t, 1, successCount, "Should successfully replay one event")
	assert.Equal(t, 0, failureCount, "Should have no failures")

	// Verify the request was received
	assert.Len(t, receivedRequests, 1, "Should have received one webhook request")

	ccbillRequest := receivedRequests[0]
	assert.Equal(t, "POST", ccbillRequest.Method)
	assert.Contains(t, ccbillRequest.URL, "/v1/webhooks/ccbill")
	assert.Equal(t, "application/x-www-form-urlencoded", ccbillRequest.ContentType)
	assert.NotEmpty(t, ccbillRequest.Body)

	// Reset for NMI test
	receivedRequests = nil

	// Test replaying a single NMI event
	successCount, failureCount, err = rs.ReplayNMIWebhooks(ctx, "recurring_subscription_add.json")
	assert.NoError(t, err)
	assert.Equal(t, 1, successCount, "Should successfully replay one NMI event")
	assert.Equal(t, 0, failureCount, "Should have no failures")

	// Verify the NMI request was received
	assert.Len(t, receivedRequests, 1, "Should have received one NMI webhook request")

	nmiRequest := receivedRequests[0]
	assert.Equal(t, "POST", nmiRequest.Method)
	assert.Contains(t, nmiRequest.URL, "/v1/webhooks/mobius")
	assert.Equal(t, "application/json", nmiRequest.ContentType)
	assert.NotEmpty(t, nmiRequest.Body)

	// Verify it's valid JSON
	var nmiPayload interface{}
	err = json.Unmarshal([]byte(nmiRequest.Body), &nmiPayload)
	assert.NoError(t, err, "NMI webhook body should be valid JSON")
}

// TestReplayService_ConcurrentReplay tests concurrent webhook replay
func TestReplayService_ConcurrentReplay(t *testing.T) {
	var requestCount atomic.Uint64

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener not permitted in this environment: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		// Add small delay to simulate processing time
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "ok"})
	}))
	server.Listener = ln
	server.Start()
	defer server.Close()

	rs := &ReplayService{
		TargetEndpoint: server.URL,
		Concurrent:     3, // Use 3 concurrent workers
		Delay:          1 * time.Millisecond,
		DryRun:         false,
		Verbose:        false,
	}

	ctx := context.Background()

	// Replay all CCBill events concurrently
	start := time.Now()
	successCount, failureCount, err := rs.ReplayCCBillWebhooks(ctx, "all")
	duration := time.Since(start)

	assert.NoError(t, err)
	assert.Greater(t, successCount, 0, "Should have successful replays")
	assert.Equal(t, 0, failureCount, "Should have no failures")
	assert.Equal(t, successCount, int(requestCount.Load()), "All requests should have been received")

	// With concurrency, it should complete faster than sequential processing
	// This is a rough check - actual timing may vary
	assert.Less(t, duration, 1*time.Second, "Concurrent processing should be relatively fast")
}

// TestReplayService_ErrorHandling tests error handling scenarios
func TestReplayService_ErrorHandling(t *testing.T) {
	// Create a server that returns errors
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener not permitted in this environment: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal server error"))
	}))
	server.Listener = ln
	server.Start()
	defer server.Close()

	rs := &ReplayService{
		TargetEndpoint: server.URL,
		Concurrent:     1,
		Delay:          1 * time.Millisecond,
		DryRun:         false,
		Verbose:        true,
	}

	ctx := context.Background()

	// Test that errors are properly handled
	successCount, failureCount, err := rs.ReplayCCBillWebhooks(ctx, "newsalesuccess.json")
	assert.NoError(t, err, "Function should not return error even if HTTP requests fail")
	assert.Equal(t, 0, successCount, "Should have no successes")
	assert.Equal(t, 1, failureCount, "Should have one failure")
}

// TestWebhookRequest represents a webhook request received by test server
type TestWebhookRequest struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	ContentType string            `json:"content_type"`
}

// TestMain runs setup and teardown for the test suite
func TestMain(m *testing.M) {
	// Verify test environment setup
	rs := &ReplayService{}
	_, err := rs.getWebhookFilesPath()
	if err != nil {
		fmt.Printf("Test setup failed: %v\n", err)
		fmt.Println("Make sure webhook test files exist in testdata/webhooks/")
		os.Exit(1)
	}

	// Run tests
	code := m.Run()
	os.Exit(code)
}
