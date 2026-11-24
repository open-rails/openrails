package handlers

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
)

// ExportBillingArchive completes the bounded snapshot before publishing HTTP
// headers. Disk spooling keeps memory bounded and lets preflight or snapshot
// failures return the ordinary error envelope instead of a partial success.
func ExportBillingArchive(r *httprequest.Request) {
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

func ImportBillingArchive(r *httprequest.Request) {
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
	r.State.MerchantChanged(mid)
	r.JSON(http.StatusOK, billing.BillingArchiveImport{
		MerchantID: mid, Digest: result.Digest, Rows: result.Rows, AlreadyImported: result.Replayed,
	})
}

func writeMerchantArchiveError(r *httprequest.Request, err error) {
	status := http.StatusInternalServerError
	detail := billing.ErrorDetails{Type: billing.ErrorTypeAPI, Code: billing.CodeBillingArchiveUnavailable, Message: "billing archive operation failed"}
	var oversized *http.MaxBytesError
	var failure *merchantarchive.Error
	switch {
	case errors.As(err, &oversized):
		status = http.StatusRequestEntityTooLarge
		detail = billing.ErrorDetails{Type: billing.ErrorTypeInvalidRequest, Code: billing.CodeRequestBodyTooLarge, Message: "billing archive exceeds its size limit"}
	case errors.As(err, &failure):
		detail.Type = billing.ErrorTypeInvalidRequest
		switch failure.Code {
		case "invalid_artifact":
			status, detail.Code, detail.Message = http.StatusBadRequest, billing.CodeBillingArchiveInvalidArtifact, "invalid, incomplete or unsupported billing archive"
		case "merchant_mismatch":
			status, detail.Code, detail.Message = http.StatusConflict, billing.CodeBillingArchiveMerchantMismatch, "archive merchant does not match the destination binding"
		case "not_empty":
			status, detail.Code, detail.Message = http.StatusConflict, billing.CodeBillingArchiveNotEmpty, "destination billing state must be empty"
		case "unsupported_state":
			status, detail.Code, detail.Message = http.StatusConflict, billing.CodeBillingArchiveUnsupportedState, "merchant has state that cannot safely be moved"
		case "platform_cards":
			status, detail.Code, detail.Message = http.StatusConflict, billing.CodeBillingArchivePlatformCards, "the merchant holds cards in the platform vault; export or remove them first"
		case "integrity":
			status, detail.Code, detail.Message = http.StatusUnprocessableEntity, billing.CodeBillingArchiveIntegrity, "billing archive integrity validation failed"
		default:
			detail.Type, detail.Code, detail.Message = billing.ErrorTypeAPI, billing.CodeBillingArchiveUnavailable, "billing archive operation failed"
		}
		if failure.Table != "" && status < 500 {
			detail.Metadata = map[string]any{"table": failure.Table, "count": failure.Count}
		}
	}
	// Do not expose PostgreSQL failing-row details or archive/provider bytes;
	// the operator's log keeps the cause.
	if err != nil && status >= http.StatusInternalServerError {
		r.Log.ErrorContext(r.Request.Context(), "billing archive failed", logx.Err(err), "code", detail.Code)
	} else if err != nil {
		r.Log.WarnContext(r.Request.Context(), "billing archive refused", logx.Err(err), "code", detail.Code)
	}
	detail.RequestID = r.RequestID()
	r.JSON(status, map[string]any{"error": detail})
}
