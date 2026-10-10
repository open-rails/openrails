package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/authkit"
	authkitgin "github.com/open-rails/authkit/adapters/gin"
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// course is the host's own: what it shows, the catalog product that sells
// it, the entitlement that unlocks it, and its video under media/.
type course struct {
	Slug, Title, Product, Entitlement, Video string
}

var courses = []course{
	{"css-101", "Intro to CSS", "course-101", "course:101", "courses/css-101.mp4"},
	{"tailwind-102", "Intro to Tailwind", "course-102", "course:102", "courses/tailwind-102.mp4"},
}

var membersQA = []string{"Grid or flexbox?", "How do I center a div?"} // members-only

// held asks OpenRails which of entitlements the signed-in user holds, in one
// read. Each AuthKit user is their own customer. A signed-out visitor holds
// nothing and costs no call.
func held(c *gin.Context, bill *openrails.Client, entitlements ...string) (map[string]bool, error) {
	claims, signedIn := verify.ClaimsFromContext(c.Request.Context()) // set by authkitgin.Optional
	if !signedIn {
		return map[string]bool{}, nil
	}
	customer, err := billing.ParseCustomerID(claims.UserID)
	if err != nil {
		return nil, err
	}
	check, err := bill.CheckEntitlements(c, billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: entitlements})
	if err != nil {
		return nil, err
	}
	return check.Entitlements, nil
}

// courseRoutes are the app's API: the course list, with prices and what the
// user owns; each course, gated; and the media its signed URLs play.
func courseRoutes(r *gin.Engine, ak *authkit.Client, bill *openrails.Client, media mediaKey) {
	// GET /api/courses?cursor=&limit= pages over the host's courses. Each page
	// costs one product read and one entitlement read, never one per course.
	r.GET("/api/courses", authkitgin.Optional(ak), func(c *gin.Context) {
		from, err1 := strconv.Atoi(c.DefaultQuery("cursor", "0"))
		limit, err2 := strconv.Atoi(c.DefaultQuery("limit", "20"))
		if err1 != nil || err2 != nil || from < 0 || limit < 1 || limit > 50 {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		page := courses[min(from, len(courses)):min(from+limit, len(courses))]
		var keys, entitlements []string
		for _, course := range page {
			keys, entitlements = append(keys, course.Product), append(entitlements, course.Entitlement)
		}
		offers, err := bill.ListOffers(c, billing.OfferListParams{Keys: keys, PageRequest: billing.PageRequest{Limit: len(keys)}})
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		owned, err := held(c, bill, entitlements...)
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		prices := map[string][]gin.H{} // by product key
		for _, product := range offers.Items {
			for _, p := range product.Prices {
				prices[product.Key] = append(prices[product.Key], gin.H{"key": p.Key, "amount": strconv.FormatInt(p.UnitAmount, 10), "currency": p.Currency, "terms": terms(p)})
			}
		}
		data := []gin.H{}
		for _, course := range page {
			data = append(data, gin.H{"slug": course.Slug, "title": course.Title, "product_key": course.Product, "owned": owned[course.Entitlement], "prices": prices[course.Product]})
		}
		var next any // null on the last page
		if from+limit < len(courses) {
			next = strconv.Itoa(from + limit)
		}
		c.JSON(http.StatusOK, gin.H{"data": data, "next_cursor": next})
	})

	// A course: its video's signed URL to a holder, 402 and where to buy it to
	// anyone else, signed out included.
	r.GET("/api/courses/:course", authkitgin.Optional(ak), func(c *gin.Context) {
		i := slices.IndexFunc(courses, func(course course) bool { return course.Slug == c.Param("course") })
		if i < 0 {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		course := courses[i]
		owned, err := held(c, bill, course.Entitlement)
		switch {
		case err != nil:
			c.AbortWithStatus(http.StatusServiceUnavailable)
		case owned[course.Entitlement]:
			c.JSON(http.StatusOK, gin.H{"video_url": media.url(course.Video)})
		default:
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "access_required", "entitlement": course.Entitlement, "buy": "/courses/" + course.Slug + "/buy"})
		}
	})

	r.GET("/api/members/qa", authkitgin.Optional(ak), func(c *gin.Context) {
		owned, err := held(c, bill, "channel:membership")
		switch {
		case err != nil:
			c.AbortWithStatus(http.StatusServiceUnavailable)
		case owned["channel:membership"]:
			c.JSON(http.StatusOK, gin.H{"questions": membersQA})
		default:
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "access_required", "entitlement": "channel:membership", "buy": "/join"})
		}
	})

	// GET /media/*path serves a file under media/ to a signed, unexpired URL;
	// ServeContent answers Range requests, so the player can seek.
	r.GET("/media/*path", func(c *gin.Context) {
		path, expires := strings.TrimPrefix(c.Param("path"), "/"), c.Query("expires")
		at, err := strconv.ParseInt(expires, 10, 64)
		if err != nil || time.Now().Unix() > at || !hmac.Equal([]byte(c.Query("sig")), []byte(media.sign(path, expires))) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		f, err := os.Open(filepath.Join("media", filepath.FromSlash(path)))
		if err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		http.ServeContent(c.Writer, c.Request, stat.Name(), stat.ModTime(), f)
	})
}

// mediaKey signs media URLs for 15 minutes, so a <video> plays them without
// a token. In production use an S3/R2 presigned GET, a CloudFront signed URL
// or ContentKit's media tokens.
type mediaKey []byte

func (k mediaKey) url(path string) string {
	expires := strconv.FormatInt(time.Now().Add(15*time.Minute).Unix(), 10)
	return "/media/" + path + "?" + url.Values{"expires": {expires}, "sig": {k.sign(path, expires)}}.Encode()
}

func (k mediaKey) sign(path, expires string) string {
	mac := hmac.New(sha256.New, k)
	mac.Write([]byte(path + "\n" + expires))
	return hex.EncodeToString(mac.Sum(nil))
}

// terms says how a price renews, or how long it grants access.
func terms(p billing.Price) string {
	switch {
	case p.BillingIntervalHours != nil:
		return fmt.Sprintf("every %d days", *p.BillingIntervalHours/24)
	case p.AccessDurationHours != nil:
		return fmt.Sprintf("for %d days", *p.AccessDurationHours/24)
	default:
		return "to keep"
	}
}

// serveApp serves the React app built into dir: its assets, and index.html
// for every page it routes itself, such as /courses/css-101/buy.
func serveApp(r *gin.Engine, dir string) {
	r.Static("/assets", dir+"/assets")
	r.NoRoute(func(c *gin.Context) { c.File(dir + "/index.html") })
}
