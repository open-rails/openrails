package guide

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails"
	openrailsgin "github.com/open-rails/openrails/adapters/gin"
)

// EmbeddedCheckoutPage serves the payment page on its own origin, ahead of
// every other route. The engine was built with Config.CheckoutOrigin and
// Deps.CheckoutAssets.
func EmbeddedCheckoutPage(router *gin.Engine, bill *openrails.Engine, format gin.LogFormatter) error {
	// guide:embedded-checkout
	origin, page := bill.CheckoutPage()
	if page == nil {
		return errors.New("no checkout page: set Config.CheckoutOrigin and Deps.CheckoutAssets")
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
