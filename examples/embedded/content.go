package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/authkit"
	authkitgin "github.com/open-rails/authkit/adapters/gin"
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// gateContent is the README's gate: the content API answers only a customer
// holding the entitlement the content needs, and tells anyone else, signed
// out included, where to buy it (402).
func gateContent(r *gin.Engine, ak *authkit.Client, bill *openrails.Client) {
	// What each course needs (catalog.yaml) and holds.
	courses := map[string]struct {
		Entitlement string
		Lessons     []string
	}{
		"css-101":      {"course:101", []string{"Selectors", "The box model", "Flexbox"}},
		"tailwind-102": {"course:102", []string{"Utility classes", "Responsive design"}},
	}
	membersQA := []string{"Grid or flexbox?", "How do I center a div?"} // members-only

	// holds reports whether the caller holds entitlement; signed out holds nothing.
	// Each AuthKit user is their own customer.
	holds := func(c *gin.Context, entitlement string) (bool, error) {
		claims, signedIn := verify.ClaimsFromContext(c.Request.Context())
		if !signedIn {
			return false, nil
		}
		customer, err := billing.ParseCustomerID(claims.UserID)
		if err != nil {
			return false, err
		}
		check, err := bill.CheckEntitlements(c, billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{entitlement}})
		if err != nil {
			return false, err
		}
		return check.Entitlements[entitlement], nil
	}

	r.GET("/api/courses/:course", authkitgin.Optional(ak), func(c *gin.Context) {
		course, exists := courses[c.Param("course")]
		if !exists {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		held, err := holds(c, course.Entitlement)
		switch {
		case err != nil:
			c.AbortWithStatus(http.StatusServiceUnavailable)
		case !held:
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "access_required", "buy": "/courses/" + c.Param("course") + "/buy"})
		default:
			c.JSON(http.StatusOK, gin.H{"lessons": course.Lessons})
		}
	})

	r.GET("/api/members/qa", authkitgin.Optional(ak), func(c *gin.Context) {
		held, err := holds(c, "channel:membership")
		switch {
		case err != nil:
			c.AbortWithStatus(http.StatusServiceUnavailable)
		case !held:
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "access_required", "buy": "/join"})
		default:
			c.JSON(http.StatusOK, gin.H{"questions": membersQA})
		}
	})
}

// serveApp serves the React app built into dir: its assets, and index.html
// for every page it routes itself, such as /courses/css-101/buy.
func serveApp(r *gin.Engine, dir string) {
	r.Static("/assets", dir+"/assets")
	r.NoRoute(func(c *gin.Context) { c.File(dir + "/index.html") })
}
