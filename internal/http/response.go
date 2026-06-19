package server

import (
	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/pkg/api"
)

// jsonError writes the canonical Stripe-style error envelope
// ({"error":{type,code,message}}) — the same shape emitted across the rest of the
// HTTP surface by httprequest.Request.ErrorJSON. The raw-gin handlers in this
// package use it instead of an ad-hoc gin.H{"error": "..."} so clients decode one
// error shape everywhere. Type and code are inferred from the status code (see
// api.SimpleErrorResponse); when a specific machine code is warranted, call
// c.JSON(code, api.NewAPIError(...).ToResponse()) directly.
func jsonError(c *gin.Context, code int, msg string) {
	c.JSON(code, api.SimpleErrorResponse(code, msg))
}
