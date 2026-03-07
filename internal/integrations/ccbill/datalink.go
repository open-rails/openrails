package ccbill

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
)

type DataLinkClient struct {
	BaseURL      string
	ClientAccNum string
	ClientSubAcc string
	Username     string
	Password     string
	DevMode      bool
	HTTPClient   *http.Client
}

const defaultDataLinkBaseURL = "https://datalink.ccbill.com"

type CCBillRecord struct {
	TransactionType string
	ClientAccNum    string
	Field2          string
	SubscriptionID  int64
	Date            string
	Username        string
	Email           string
	Status          string
	RebillDate      string
	ExpiryDate      string
}

func NewDataLinkClient(cfg *config.CCBillConfig) *DataLinkClient {
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
	// Build base URL
	apiURL := fmt.Sprintf("%s/data/main.cgi", c.BaseURL)

	// Create form data for POST request
	formData := url.Values{}
	formData.Set("transactionTypes", "ACTIVEMEMBERS")
	formData.Set("clientAccnum", c.ClientAccNum)
	formData.Set("username", c.Username)
	formData.Set("password", c.Password)

	if c.ClientSubAcc != "" {
		formData.Set("clientSubacc", c.ClientSubAcc)
	}

	/*if c.DevMode {
		formData.Set("testMode", "1")
	}*/

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
			return nil, fmt.Errorf("creating request: %w", err)
		}

		// Set proper headers
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "DoujinsApp/1.0")

		resp, err = c.HTTPClient.Do(req)
		if err != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"try":   tries,
				"error": err.Error(),
			}).Warn("Request failed")

			if tries == maxRetries {
				return nil, fmt.Errorf("failed after %d retries: %w", maxRetries, err)
			}
			time.Sleep(time.Duration(tries) * time.Second) // Exponential backoff
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			break
		}

		// Handle authentication errors specifically
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("authentication failed: invalid credentials")
		}
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("access forbidden: check client account permissions")
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"status_code": resp.StatusCode,
			"try":         tries,
		}).Warn("Non-200 response from CCBill")

		if tries == maxRetries {
			return nil, fmt.Errorf("failed after %d retries, last status: %d", maxRetries, resp.StatusCode)
		}
		time.Sleep(time.Duration(tries) * time.Second) // Exponential backoff
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	content := string(body)

	// Check for actual CCBill error responses (more specific error detection)
	// CCBill errors typically start with specific error messages, not CSV data
	if !strings.HasPrefix(content, `"ACTIVEMEMBERS"`) &&
		(strings.HasPrefix(strings.ToLower(content), "error") ||
			strings.HasPrefix(strings.ToLower(content), "invalid") ||
			strings.Contains(strings.ToLower(content), "authentication failed") ||
			strings.Contains(strings.ToLower(content), "access denied")) {
		log.WithContext(ctx).WithField("response_length", len(content)).Error("Error response from CCBill")
		return nil, fmt.Errorf("ccbill api returned an error payload")
	}

	if !strings.HasPrefix(content, `"ACTIVEMEMBERS"`) {
		log.WithContext(ctx).WithField("response_length", len(content)).Error("Invalid response format from CCBill")
		return nil, fmt.Errorf("invalid response format from CCBill: expected ACTIVEMEMBERS data")
	}

	return c.ProcessCSVData(ctx, content)
}

func (c *DataLinkClient) ProcessCSVData(ctx context.Context, csvData string) ([]CCBillRecord, error) {
	reader := csv.NewReader(strings.NewReader(csvData))
	reader.Comma = ','
	reader.Comment = 0
	reader.FieldsPerRecord = -1

	var records []CCBillRecord
	recordNumber := 0

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
			log.WithContext(ctx).WithFields(log.Fields{
				"record_number": recordNumber,
				"fields":        len(record),
			}).Warn("Incomplete CSV record, skipping")
			continue
		}

		ccbillRecord, err := c.parseRecord(record)
		if err != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"record_number": recordNumber,
				"error":         err.Error(),
			}).Warn("Failed to parse record, skipping")
			continue
		}

		records = append(records, *ccbillRecord)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"total_records":  recordNumber,
		"parsed_records": len(records),
	}).Info("Completed CSV processing")

	return records, nil
}

func (c *DataLinkClient) parseRecord(record []string) (*CCBillRecord, error) {
	subID, err := strconv.ParseInt(record[3], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing subscription ID '%s': %w", record[3], err)
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

func (c *DataLinkClient) ParseCCBillDate(dateStr string) (*time.Time, error) {
	if dateStr == "" {
		return nil, nil
	}

	// Try common CCBill date formats
	formats := []string{
		"2006-01-02",
		"01/02/2006",
		"2006-01-02 15:04:05",
		"01/02/2006 15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05.000Z",
	}

	for _, format := range formats {
		if parsed, err := time.Parse(format, dateStr); err == nil {
			return &parsed, nil
		}
	}

	return nil, fmt.Errorf("unable to parse CCBill date: %s", dateStr)
}

func (c *DataLinkClient) MapCCBillStatusToInternal(ccbillStatus string) string {
	switch ccbillStatus {
	case "ACTIVE":
		return "active"
	case "EXPIRED":
		return "expired"
	case "CANCELLED":
		return "cancelled"
	case "PENDING":
		return "pending"
	default:
		log.WithField("ccbill_status", ccbillStatus).Warn("Unknown CCBill status, defaulting to pending")
		return "pending"
	}
}

func (c *DataLinkClient) ValidateRecord(record *CCBillRecord) error {
	if record.SubscriptionID <= 0 {
		return fmt.Errorf("invalid subscription ID: %d", record.SubscriptionID)
	}
	if record.Email == "" {
		return fmt.Errorf("empty email address")
	}
	if record.Status == "" {
		return fmt.Errorf("empty status")
	}

	return nil
}
