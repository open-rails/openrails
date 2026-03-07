package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ReplayService replays saved webhook payloads against a target endpoint.
type ReplayService struct {
	TargetEndpoint string        // Base URL for webhook endpoints (e.g., http://localhost:2053)
	Concurrent     int           // Number of concurrent requests
	Delay          time.Duration // Delay between requests
	DryRun         bool          // Validate payloads without sending
	Verbose        bool          // Enable detailed logging
}

// ReplayResult represents the result of a webhook replay attempt.
type ReplayResult struct {
	EventFile    string `json:"event_file"`
	Processor    string `json:"processor"`
	EventType    string `json:"event_type,omitempty"`
	Success      bool   `json:"success"`
	StatusCode   int    `json:"status_code,omitempty"`
	ResponseTime string `json:"response_time,omitempty"`
	Error        string `json:"error,omitempty"`
}

// getProjectRoot returns the project root directory by finding go.mod.
func getProjectRoot() (string, error) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("failed to get current file path")
	}

	dir := filepath.Dir(currentFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("go.mod not found")
}

// getWebhookFilesPath returns the path to webhook test files.
func (rs *ReplayService) getWebhookFilesPath() (string, error) {
	projectRoot, err := getProjectRoot()
	if err != nil {
		return "", fmt.Errorf("failed to find project root: %w", err)
	}

	webhookPath := filepath.Join(projectRoot, "testdata", "webhooks")
	if _, err := os.Stat(webhookPath); os.IsNotExist(err) {
		return "", fmt.Errorf("webhook testdata directory not found at %s", webhookPath)
	}

	return webhookPath, nil
}

// loadWebhookEvents loads webhook event files from the specified processor directory.
func (rs *ReplayService) loadWebhookEvents(processor, eventFilter string) ([]string, error) {
	webhookPath, err := rs.getWebhookFilesPath()
	if err != nil {
		return nil, err
	}

	processorPath := filepath.Join(webhookPath, processor)
	if _, err := os.Stat(processorPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("processor directory not found: %s", processorPath)
	}

	var eventFiles []string

	if eventFilter == "all" {
		files, err := os.ReadDir(processorPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read processor directory %s: %w", processorPath, err)
		}

		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), ".json") {
				eventFiles = append(eventFiles, filepath.Join(processorPath, file.Name()))
			}
		}
	} else {
		eventFile := filepath.Join(processorPath, eventFilter)
		if _, err := os.Stat(eventFile); os.IsNotExist(err) {
			return nil, fmt.Errorf("event file not found: %s", eventFile)
		}
		eventFiles = append(eventFiles, eventFile)
	}

	if len(eventFiles) == 0 {
		return nil, fmt.Errorf("no webhook event files found in %s", processorPath)
	}

	return eventFiles, nil
}

// validateWebhookPayload validates that a webhook payload is valid JSON.
func (rs *ReplayService) validateWebhookPayload(filePath string) (*ReplayResult, error) {
	result := &ReplayResult{
		EventFile: filepath.Base(filePath),
		Processor: filepath.Base(filepath.Dir(filePath)),
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		result.Error = fmt.Sprintf("Failed to read file: %v", err)
		return result, nil
	}

	var payload interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		result.Error = fmt.Sprintf("Invalid JSON: %v", err)
		return result, nil
	}

	if result.Processor == "nmi" || result.Processor == "mobius" {
		if payloadArray, ok := payload.([]interface{}); ok && len(payloadArray) > 0 {
			if firstEvent, ok := payloadArray[0].(map[string]interface{}); ok {
				if eventType, exists := firstEvent["event_type"]; exists {
					if eventTypeStr, ok := eventType.(string); ok {
						result.EventType = eventTypeStr
					}
				}
			}
		} else if payloadMap, ok := payload.(map[string]interface{}); ok {
			if eventType, exists := payloadMap["event_type"]; exists {
				if eventTypeStr, ok := eventType.(string); ok {
					result.EventType = eventTypeStr
				}
			}
		}
	}

	result.Success = true
	return result, nil
}

// replayWebhookEvent sends a webhook event to the target endpoint.
func (rs *ReplayService) replayWebhookEvent(ctx context.Context, filePath string) (*ReplayResult, error) {
	result := &ReplayResult{
		EventFile: filepath.Base(filePath),
		Processor: filepath.Base(filepath.Dir(filePath)),
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		result.Error = fmt.Sprintf("Failed to read file: %v", err)
		return result, nil
	}

	var payload interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		result.Error = fmt.Sprintf("Invalid JSON: %v", err)
		return result, nil
	}

	if result.Processor == "nmi" || result.Processor == "mobius" {
		if payloadArray, ok := payload.([]interface{}); ok && len(payloadArray) > 0 {
			if firstEvent, ok := payloadArray[0].(map[string]interface{}); ok {
				if eventType, exists := firstEvent["event_type"]; exists {
					if eventTypeStr, ok := eventType.(string); ok {
						result.EventType = eventTypeStr
					}
				}
			}
		} else if payloadMap, ok := payload.(map[string]interface{}); ok {
			if eventType, exists := payloadMap["event_type"]; exists {
				if eventTypeStr, ok := eventType.(string); ok {
					result.EventType = eventTypeStr
				}
			}
		}
	}

	provider := result.Processor
	if provider == "nmi" {
		provider = "mobius"
	}
	// Canonical standalone webhook path: /v1/webhooks/:provider
	webhookURL, err := url.JoinPath(rs.TargetEndpoint, "v1", "webhooks", provider)
	if err != nil {
		result.Error = fmt.Sprintf("Failed to build webhook URL: %v", err)
		return result, nil
	}

	var requestBody io.Reader
	var contentType string

	if result.Processor == "ccbill" {
		formData := url.Values{}
		if payloadMap, ok := payload.(map[string]interface{}); ok {
			for key, value := range payloadMap {
				formData.Set(key, fmt.Sprintf("%v", value))
			}
		}
		requestBody = strings.NewReader(formData.Encode())
		contentType = "application/x-www-form-urlencoded"
	} else {
		requestBody = bytes.NewReader(data)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, requestBody)
	if err != nil {
		result.Error = fmt.Sprintf("Failed to create request: %v", err)
		return result, nil
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "webhook-replay-tool/1.0")

	startTime := time.Now()
	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	responseTime := time.Since(startTime)
	result.ResponseTime = responseTime.String()

	if err != nil {
		result.Error = fmt.Sprintf("Request failed: %v", err)
		return result, nil
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode

	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.Success = true
	} else {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	return result, nil
}

// processWebhookEvents processes a list of webhook events with concurrency control.
func (rs *ReplayService) processWebhookEvents(ctx context.Context, eventFiles []string, processor string) (int, int, error) {
	if len(eventFiles) == 0 {
		return 0, 0, nil
	}

	workCh := make(chan string, len(eventFiles))
	resultCh := make(chan *ReplayResult, len(eventFiles))

	for _, eventFile := range eventFiles {
		workCh <- eventFile
	}
	close(workCh)

	var wg sync.WaitGroup
	numWorkers := rs.Concurrent
	if numWorkers <= 0 {
		numWorkers = 1
	}
	if numWorkers > len(eventFiles) {
		numWorkers = len(eventFiles)
	}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for eventFile := range workCh {
				if rs.Delay > 0 {
					time.Sleep(rs.Delay)
				}

				var result *ReplayResult
				var err error

				if rs.DryRun {
					result, err = rs.validateWebhookPayload(eventFile)
				} else {
					result, err = rs.replayWebhookEvent(ctx, eventFile)
				}

				if err != nil {
					result = &ReplayResult{
						EventFile: filepath.Base(eventFile),
						Processor: processor,
						Error:     err.Error(),
					}
				}

				resultCh <- result

				if rs.Verbose {
					if result.Success {
						if rs.DryRun {
							fmt.Printf("    SUCCESS %s: Valid payload", result.EventFile)
						} else {
							fmt.Printf("    SUCCESS %s: %d (%s)", result.EventFile, result.StatusCode, result.ResponseTime)
						}
						if result.EventType != "" {
							fmt.Printf(" [%s]", result.EventType)
						}
						fmt.Println()
					} else {
						fmt.Printf("    FAILED %s: %s\n", result.EventFile, result.Error)
					}
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	var successCount, failureCount int
	for result := range resultCh {
		if result.Success {
			successCount++
		} else {
			failureCount++
			if !rs.Verbose {
				fmt.Printf("    FAILED %s: %s\n", result.EventFile, result.Error)
			}
		}
	}

	return successCount, failureCount, nil
}

// ReplayCCBillWebhooks replays CCBill webhook events.
func (rs *ReplayService) ReplayCCBillWebhooks(ctx context.Context, eventFilter string) (int, int, error) {
	eventFiles, err := rs.loadWebhookEvents("ccbill", eventFilter)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to load CCBill webhook events: %w", err)
	}

	if rs.Verbose {
		fmt.Printf("  Found %d CCBill event file(s)\n", len(eventFiles))
	}

	return rs.processWebhookEvents(ctx, eventFiles, "ccbill")
}

// ReplayNMIWebhooks replays NMI webhook events.
func (rs *ReplayService) ReplayNMIWebhooks(ctx context.Context, eventFilter string) (int, int, error) {
	eventFiles, err := rs.loadWebhookEvents("mobius", eventFilter)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to load NMI webhook events: %w", err)
	}

	if rs.Verbose {
		fmt.Printf("  Found %d NMI event file(s)\n", len(eventFiles))
	}

	return rs.processWebhookEvents(ctx, eventFiles, "nmi")
}

// ReplayEvent replays a single webhook event to the target URL.
func ReplayEvent(ctx context.Context, processor, eventFile, targetURL string) error {
	rs := &ReplayService{
		TargetEndpoint: targetURL,
		Concurrent:     1,
		Delay:          0,
		DryRun:         false,
		Verbose:        false,
	}

	var failures int
	var err error

	switch processor {
	case "ccbill":
		_, failures, err = rs.ReplayCCBillWebhooks(ctx, eventFile)
	case "nmi", "mobius":
		_, failures, err = rs.ReplayNMIWebhooks(ctx, eventFile)
	default:
		return fmt.Errorf("invalid processor '%s'. Must be: ccbill, mobius, or nmi", processor)
	}

	if err != nil {
		return err
	}

	if failures > 0 {
		return fmt.Errorf("webhook replay failed for %s/%s", processor, eventFile)
	}

	return nil
}

// ReplayAllEvents replays all webhook events for a processor.
func ReplayAllEvents(ctx context.Context, processor, targetURL string) error {
	return ReplayEvent(ctx, processor, "all", targetURL)
}

// ValidateEvent validates a webhook event payload without sending HTTP requests.
func ValidateEvent(processor, eventFile string) error {
	rs := &ReplayService{
		DryRun:  true,
		Verbose: false,
	}

	var failures int
	var err error

	switch processor {
	case "ccbill":
		_, failures, err = rs.ReplayCCBillWebhooks(context.Background(), eventFile)
	case "nmi", "mobius":
		_, failures, err = rs.ReplayNMIWebhooks(context.Background(), eventFile)
	default:
		return fmt.Errorf("invalid processor '%s'. Must be: ccbill, mobius, or nmi", processor)
	}

	if err != nil {
		return err
	}

	if failures > 0 {
		return fmt.Errorf("webhook validation failed for %s/%s", processor, eventFile)
	}

	return nil
}

// ValidateAllEvents validates all webhook events for a processor.
func ValidateAllEvents(processor string) error {
	return ValidateEvent(processor, "all")
}

// LoadTestWebhookPayload loads a test webhook payload from testdata.
func LoadTestWebhookPayload(processor, eventFile string) (string, error) {
	projectRoot, err := getProjectRoot()
	if err != nil {
		return "", fmt.Errorf("failed to find project root: %w", err)
	}

	payloadPath := filepath.Join(projectRoot, "testdata", "webhooks", processor, eventFile)
	data, err := os.ReadFile(payloadPath)
	if err != nil {
		return "", fmt.Errorf("failed to read payload file %s: %w", payloadPath, err)
	}

	return string(data), nil
}
