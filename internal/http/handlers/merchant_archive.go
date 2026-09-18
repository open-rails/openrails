package handlers

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ExportMerchantBilling completes the bounded snapshot before publishing HTTP
// headers. Disk spooling keeps memory bounded and lets preflight or snapshot
// failures return the ordinary error envelope instead of a partial success.
func ExportMerchantBilling(r *httprequest.Request) {
	mid, err := merchant.Require(r.Request.Context())
	if err != nil || r.State == nil || r.State.DB == nil {
		writeMerchantArchiveError(r, nil)
		return
	}
	file, err := os.CreateTemp("", "openrails-billing-archive-*")
	if err != nil {
		writeMerchantArchiveError(r, nil)
		return
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if err := merchantarchive.Export(r.Request.Context(), r.State.DB, mid, file); err != nil {
		writeMerchantArchiveError(r, err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		writeMerchantArchiveError(r, nil)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeMerchantArchiveError(r, nil)
		return
	}
	r.SetHeader("Content-Type", "application/x-ndjson")
	r.SetHeader("Content-Disposition", `attachment; filename="merchant-billing.ndjson"`)
	r.SetHeader("Cache-Control", "no-store")
	r.SetHeader("Content-Length", strconv.FormatInt(info.Size(), 10))
	if err := r.Stream(http.StatusOK, file); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func ImportMerchantBilling(r *httprequest.Request) {
	mid, err := merchant.Require(r.Request.Context())
	if err != nil || r.State == nil || r.State.DB == nil {
		writeMerchantArchiveError(r, nil)
		return
	}
	result, err := merchantarchive.Restore(r.Request.Context(), r.State.DB, mid, r.Request.Body)
	if err != nil {
		writeMerchantArchiveError(r, err)
		return
	}
	r.JSON(http.StatusOK, openrails.MerchantBillingImportResult{
		MerchantID: mid, Digest: result.Digest, Rows: result.Rows, AlreadyImported: result.Replayed,
	})
}

func writeMerchantArchiveError(r *httprequest.Request, err error) {
	status := http.StatusInternalServerError
	detail := openrails.ErrorDetails{Type: "api_error", Code: "billing_archive_unavailable", Message: "billing archive operation failed"}
	var oversized *http.MaxBytesError
	var failure *merchantarchive.Error
	switch {
	case errors.As(err, &oversized):
		status = http.StatusRequestEntityTooLarge
		detail = openrails.ErrorDetails{Type: "invalid_request_error", Code: openrails.CodeRequestBodyTooLarge, Message: "billing archive exceeds its size limit"}
	case errors.As(err, &failure):
		detail.Type = "invalid_request_error"
		detail.Code = "billing_archive_" + failure.Code
		switch failure.Code {
		case "invalid_artifact":
			status, detail.Message = http.StatusBadRequest, "invalid, incomplete or unsupported billing archive"
		case "merchant_mismatch":
			status, detail.Message = http.StatusConflict, "archive merchant does not match the destination binding"
		case "not_empty":
			status, detail.Message = http.StatusConflict, "destination billing state must be empty"
		case "unsupported_state":
			status, detail.Message = http.StatusConflict, "merchant has state that cannot safely be moved"
		case "integrity":
			status, detail.Message = http.StatusUnprocessableEntity, "billing archive integrity validation failed"
		default:
			detail.Type, detail.Code, detail.Message = "api_error", "billing_archive_unavailable", "billing archive operation failed"
		}
		if failure.Table != "" && status < 500 {
			detail.Metadata = map[string]any{"table": failure.Table, "count": failure.Count}
		}
	}
	// Do not expose PostgreSQL failing-row details or archive/provider bytes.
	detail.RequestID = r.RequestID()
	r.JSON(status, map[string]any{"error": detail})
}
