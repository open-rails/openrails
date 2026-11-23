package guide

import (
	"io/fs"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails"
	openrailsgin "github.com/open-rails/openrails/adapters/gin"
)

// EmbeddedCheckoutPage serves the payment page on its own origin, ahead of
// every other route. assets is the built web/pay page.
func EmbeddedCheckoutPage(router *gin.Engine, bill *openrails.Engine, assets fs.FS, format gin.LogFormatter) error {
	// guide:embedded-checkout
	origin, page, err := bill.CheckoutPage("https://pay.example.com", assets)
	if err != nil {
		return err
	}
	pay, err := openrailsgin.CheckoutOrigin(origin, page)
	if err != nil {
		return err
	}
	router.Use(pay) // before other middleware and routes
	router.Use(gin.LoggerWithFormatter(openrailsgin.RedactLog(format)))
	// guide:end
	return nil
}
