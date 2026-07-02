package ccbill

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

type DataLinkClient struct {
	BaseURL      string
	ClientAccNum string
	ClientSubAcc string
	Username     string
	Password     string
	DevMode      bool
	// ReadOnly blocks every SMS mutation (CancelSubscription) at the transport
	// with ErrProviderReadOnly; reads stay available. Set from
	// cfg.IsProviderReadOnly() in build_runtime (mode=readonly, #346).
	ReadOnly   bool
	HTTPClient *http.Client
}

const defaultDataLinkBaseURL = "https://datalink.ccbill.com"

const maxDataLinkResponseBytes = 25 << 20

type CCBillRecord struct {
	TransactionType string
	ClientAccNum    string
	Field2          string
	SubscriptionID  string
	Date            string
	Username        string
	Email           string
	Status          string
	RebillDate      string
	ExpiryDate      string
}

func NewDataLinkClient(cfg *config.CCBillConfig) *DataLinkClient {
	cfg = requireConfig(cfg)
	return &DataLinkClient{
		BaseURL:      defaultDataLinkBaseURL,
		ClientAccNum: cfg.ClientAccNum,
		ClientSubAcc: cfg.ClientSubAcc,
		Username:     cfg.DataLinkUsername,
		Password:     cfg.DataLinkPassword,
		DevMode:      cfg.TestMode,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Minute,
		},
	}
}

func (c *DataLinkClient) FetchActiveMembers(ctx context.Context) ([]CCBillRecord, error) {
	formData := url.Values{}
	formData.Set("transactionTypes", "ACTIVEMEMBERS")

	content, err := c.fetchDataLink(ctx, formData)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(content, `"ACTIVEMEMBERS"`) {
		log.WithContext(ctx).WithField("response_length", len(content)).Error("Invalid response format from CCBill")
		return nil, fmt.Errorf("invalid response format from CCBill: expected ACTIVEMEMBERS data")
	}

	return c.ProcessCSVData(ctx, content)
}

// fetchDataLink POSTs a DataLink request to /data/main.cgi with the shared
// auth/test-mode form fields merged in, retries transient failures, and
// returns the raw response body after rejecting error payloads. extra carries
// the report-specific fields (transactionTypes, startTime, endTime, ...).
func (c *DataLinkClient) fetchDataLink(ctx context.Context, extra url.Values) (string, error) {
	apiURL := fmt.Sprintf("%s/data/main.cgi", c.BaseURL)

	formData := url.Values{}
	for k, vs := range extra {
		for _, v := range vs {
			formData.Add(k, v)
		}
	}
	formData.Set("clientAccnum", c.ClientAccNum)
	formData.Set("username", c.Username)
	formData.Set("password", c.Password)

	if c.ClientSubAcc != "" {
		formData.Set("clientSubacc", c.ClientSubAcc)
	}
	if c.DevMode {
		formData.Set("testMode", "1")
	}

	var resp *http.Response
	var err error
	maxRetries := 5

	for tries := 1; tries <= maxRetries; tries++ {
		log.WithContext(ctx).WithFields(log.Fields{
			"try":      tries,
			"endpoint": apiURL,
		}).Info("Requesting data from CCBill DataLink API (POST)")

		// Create POST request with form data in body
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(formData.Encode()))
		if err != nil {
			return "", fmt.Errorf("creating request: %w", err)
		}

		// Set proper headers
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "OpenRails/1.0")

		resp, err = c.HTTPClient.Do(req)
		if err != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"try":   tries,
				"error": err.Error(),
			}).Warn("Request failed")

			if tries == maxRetries {
				return "", fmt.Errorf("failed after %d retries: %w", maxRetries, err)
			}
			if err := sleepWithContext(ctx, time.Duration(tries)*time.Second); err != nil {
				return "", err
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			break
		}

		// Handle authentication errors specifically
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return "", fmt.Errorf("authentication failed: invalid credentials")
		}
		if resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			return "", fmt.Errorf("access forbidden: check client account permissions")
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"status_code": resp.StatusCode,
			"try":         tries,
		}).Warn("Non-200 response from CCBill")

		if tries == maxRetries {
			resp.Body.Close()
			return "", fmt.Errorf("failed after %d retries, last status: %d", maxRetries, resp.StatusCode)
		}
		resp.Body.Close()
		if err := sleepWithContext(ctx, time.Duration(tries)*time.Second); err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDataLinkResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}
	if len(body) > maxDataLinkResponseBytes {
		return "", fmt.Errorf("ccbill datalink response exceeded %d bytes", maxDataLinkResponseBytes)
	}

	content := string(body)

	// Check for actual CCBill error responses (more specific error detection)
	// CCBill errors typically start with specific error messages, not CSV data
	if !strings.HasPrefix(content, `"`) &&
		(strings.HasPrefix(strings.ToLower(content), "error") ||
			strings.HasPrefix(strings.ToLower(content), "invalid") ||
			strings.Contains(strings.ToLower(content), "authentication failed") ||
			strings.Contains(strings.ToLower(content), "access denied")) {
		log.WithContext(ctx).WithField("response_length", len(content)).Error("Error response from CCBill")
		return "", fmt.Errorf("ccbill api returned an error payload")
	}

	return content, nil
}

func (c *DataLinkClient) ProcessCSVData(ctx context.Context, csvData string) ([]CCBillRecord, error) {
	reader := csv.NewReader(strings.NewReader(csvData))
	reader.Comma = ','
	reader.Comment = 0
	reader.FieldsPerRecord = -1

	var records []CCBillRecord
	recordNumber := 0
	skippedRecords := 0

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CSV record %d: %w", recordNumber, err)
		}

		recordNumber++

		if len(record) < 10 {
			skippedRecords++
			log.WithContext(ctx).WithFields(log.Fields{
				"record_number": recordNumber,
				"fields":        len(record),
			}).Warn("Incomplete CSV record, skipping")
			continue
		}

		ccbillRecord, err := c.parseRecord(record)
		if err != nil {
			skippedRecords++
			log.WithContext(ctx).WithFields(log.Fields{
				"record_number": recordNumber,
				"error":         err.Error(),
			}).Warn("Failed to parse record, skipping")
			continue
		}

		records = append(records, *ccbillRecord)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"total_records":   recordNumber,
		"parsed_records":  len(records),
		"skipped_records": skippedRecords,
	}).Info("Completed CSV processing")
	if skippedRecords > 0 {
		return nil, fmt.Errorf("ccbill datalink response contained %d skipped records", skippedRecords)
	}

	return records, nil
}

func (c *DataLinkClient) parseRecord(record []string) (*CCBillRecord, error) {
	subID := strings.TrimSpace(record[3])
	if subID == "" {
		return nil, fmt.Errorf("subscription ID is required")
	}

	return &CCBillRecord{
		TransactionType: record[0],
		ClientAccNum:    record[1],
		Field2:          record[2],
		SubscriptionID:  subID,
		Date:            record[4],
		Username:        record[5],
		Email:           record[6],
		Status:          record[7],
		RebillDate:      record[8],
		ExpiryDate:      record[9],
	}, nil
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func IsDataLinkActiveStatus(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "1", "Y", "YES", "ACTIVE", "A":
		return true
	default:
		return false
	}
}

func DataLinkPaidThrough(record CCBillRecord, now time.Time) (time.Time, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	for _, raw := range []string{record.ExpiryDate, record.RebillDate} {
		parsed, err := timeutil.ParseFirstUTC(strings.TrimSpace(raw), "2006-01-02", "01/02/2006", time.RFC3339)
		if err != nil {
			continue
		}
		paidThrough := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 23, 59, 59, 0, time.UTC)
		if paidThrough.After(now.UTC()) {
			return paidThrough, true
		}
	}
	return time.Time{}, false
}

func (c *DataLinkClient) ValidateConfig() error {
	if c.ClientAccNum == "" {
		return fmt.Errorf("CCBill datalink client account number is required")
	}
	if c.Username == "" {
		return fmt.Errorf("CCBill datalink username is required")
	}
	if c.Password == "" {
		return fmt.Errorf("CCBill datalink password is required")
	}
	return nil
}
