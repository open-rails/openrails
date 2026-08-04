package basistheory

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Batch Account Updater: the FPAN refresh path for merchants that charge the
// detokenized PAN (us, on NMI). Flow: create job -> upload request CSV to the
// returned upload_url (expires 1h) -> `account-updater.job.completed` webhook
// -> download result CSV -> UPD_* rows rotate rail_method_ref to new_token.
// Cadence is caller-owned and must be watermarked/ahead-of-renewal windows —
// never an all-instruments sweep.

// Account Updater result codes (result CSV v1.2).
const (
	AUUpdatedPAN     = "UPD_PAN"
	AUUpdatedExpDate = "UPD_EXP_DATE"
	AUNoUpdate       = "NO_UPDATE"
	AUClosedAccount  = "WRN_CLOSED_ACCOUNT"
)

type AccountUpdaterJob struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id"`
	State       string     `json:"state"`
	UploadURL   string     `json:"upload_url"`
	DownloadURL string     `json:"download_url"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// AccountUpdaterRequestRow is one request-CSV line. Token is required;
// expiration and merchant id are optional per the CSV contract.
type AccountUpdaterRequestRow struct {
	Token           string
	ExpirationYear  string
	ExpirationMonth string
	MerchantID      string
}

// AccountUpdaterResultRow is one result-CSV (v1.2) line, verbatim. NewToken
// non-empty means BT minted a NEW token id: rotate every rail_method_ref.
type AccountUpdaterResultRow struct {
	Token              string
	ExpirationYear     string
	ExpirationMonth    string
	NewToken           string
	NewExpirationYear  string
	NewExpirationMonth string
	ResultCode         string
	NewFingerprint     string
	NewBrand           string
	NewLast4           string
}

func (c *Client) CreateAccountUpdaterJob(ctx context.Context, idempotencyKey string) (*AccountUpdaterJob, error) {
	var out AccountUpdaterJob
	if err := c.doJSON(ctx, http.MethodPost, "/account-updater/jobs", map[string]any{}, idempotencyKey, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.UploadURL) == "" {
		return nil, errors.New("basistheory: account updater job response missing upload_url")
	}
	return &out, nil
}

func (c *Client) GetAccountUpdaterJob(ctx context.Context, id string) (*AccountUpdaterJob, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("basistheory: account updater job id is required")
	}
	var out AccountUpdaterJob
	if err := c.doJSON(ctx, http.MethodGet, "/account-updater/jobs/"+url.PathEscape(id), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadAccountUpdaterCSV renders and PUTs the request CSV to the job's
// pre-signed upload_url (a mutation: blocked by the readonly transport).
func (c *Client) UploadAccountUpdaterCSV(ctx context.Context, uploadURL string, rows []AccountUpdaterRequestRow) error {
	uploadURL = strings.TrimSpace(uploadURL)
	if uploadURL == "" {
		return errors.New("basistheory: upload url is required")
	}
	// SEC-24 item 5 (same class): this URL comes from the PROVIDER's job
	// payload. It is a pre-signed object-store URL, so its host cannot be
	// pinned — but it must at least be publicly routable, or the job payload
	// becomes a lever to POST a CSV of card tokens at an internal service.
	if err := c.outbound.ValidateURL(uploadURL); err != nil {
		return fmt.Errorf("basistheory: account updater upload url refused: %w", err)
	}
	if len(rows) == 0 {
		return errors.New("basistheory: account updater upload requires at least one row")
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"token", "expiration_year", "expiration_month", "merchant_id"})
	for _, r := range rows {
		if strings.TrimSpace(r.Token) == "" {
			return errors.New("basistheory: account updater row missing token")
		}
		_ = w.Write([]string{r.Token, r.ExpirationYear, r.ExpirationMonth, r.MerchantID})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("basistheory: render account updater csv: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/csv")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("basistheory: account updater csv upload failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// DownloadAccountUpdaterResults GETs and parses the completed job's result CSV.
func (c *Client) DownloadAccountUpdaterResults(ctx context.Context, downloadURL string) ([]AccountUpdaterResultRow, error) {
	downloadURL = strings.TrimSpace(downloadURL)
	if downloadURL == "" {
		return nil, errors.New("basistheory: download url is required")
	}
	if err := c.outbound.ValidateURL(downloadURL); err != nil {
		return nil, fmt.Errorf("basistheory: account updater download url refused: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("basistheory: account updater result download failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return ParseAccountUpdaterResults(resp.Body)
}

// ParseAccountUpdaterResults parses a v1.2 result CSV by header name (column
// order is not assumed). Rows are recorded verbatim (#651).
func ParseAccountUpdaterResults(r io.Reader) ([]AccountUpdaterResultRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("basistheory: read result csv header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}
	if _, ok := col["token"]; !ok {
		return nil, fmt.Errorf("basistheory: result csv missing required 'token' column (header %v)", header)
	}
	field := func(rec []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	var out []AccountUpdaterResultRow
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("basistheory: read result csv row: %w", err)
		}
		out = append(out, AccountUpdaterResultRow{
			Token:              field(rec, "token"),
			ExpirationYear:     field(rec, "expiration_year"),
			ExpirationMonth:    field(rec, "expiration_month"),
			NewToken:           field(rec, "new_token"),
			NewExpirationYear:  field(rec, "new_expiration_year"),
			NewExpirationMonth: field(rec, "new_expiration_month"),
			ResultCode:         field(rec, "result_code"),
			NewFingerprint:     field(rec, "new_fingerprint"),
			NewBrand:           field(rec, "new_brand"),
			NewLast4:           field(rec, "new_last4"),
		})
	}
}
