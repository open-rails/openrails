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
// it and the entitlement that product grants, and its video under media/. A
// members-only video has no product: only the membership unlocks it.
type course struct {
	Slug, Title, Product, Entitlement, Video string
	MembersOnly                              bool
}

var courses = []course{
	{Slug: "css-101", Title: "Intro to CSS", Product: "course-101", Entitlement: "course:101", Video: "courses/css-101.mp4"},
	{Slug: "tailwind-102", Title: "Intro to Tailwind", Product: "course-102", Entitlement: "course:102", Video: "courses/tailwind-102.mp4"},
	{Slug: "live-qa", Title: "Live Q&A", Video: "courses/live-qa.mp4", MembersOnly: true},
}

// The channel membership unlocks every video, members-only ones included.
const membership, membershipProduct = "channel:membership", "channel-membership"

// unlock lists the entitlements that unlock c: any one will do.
func (c course) unlock() []string {
	if c.MembersOnly {
		return []string{membership}
	}
	return []string{c.Entitlement, membership}
}

// product is the catalog product the store sells c with.
func (c course) product() string {
	if c.MembersOnly {
		return membershipProduct
	}
	return c.Product
}

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
// user owns; each video, gated; and the media its signed URLs play.
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
		keys, entitlements := []string{}, []string{membership} // the membership unlocks every course
		for _, course := range page {
			keys = append(keys, course.product())
			if !course.MembersOnly {
				entitlements = append(entitlements, course.Entitlement)
			}
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
			data = append(data, gin.H{"slug": course.Slug, "title": course.Title, "members_only": course.MembersOnly,
				"owned": anyHeld(owned, course.unlock()), "product_key": course.product(), "prices": prices[course.product()]})
		}
		var next any // null on the last page
		if from+limit < len(courses) {
			next = strconv.Itoa(from + limit)
		}
		c.JSON(http.StatusOK, gin.H{"data": data, "next_cursor": next})
	})

	// A video: its signed URL to whoever holds any key that unlocks it, 402
	// with the products on sale that unlock it and where to buy one to anyone
	// else, signed out included.
	r.GET("/api/courses/:course", authkitgin.Optional(ak), func(c *gin.Context) {
		i := slices.IndexFunc(courses, func(course course) bool { return course.Slug == c.Param("course") })
		if i < 0 {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		course := courses[i]
		owned, err := held(c, bill, course.unlock()...)
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if anyHeld(owned, course.unlock()) {
			c.JSON(http.StatusOK, gin.H{"video_url": media.url(course.Video)})
			return
		}
		// Everything on sale granting a key that unlocks it: the course, the
		// bundle, the membership. The buy page offers them by product key.
		offers, err := bill.ListOffers(c, billing.OfferListParams{Entitlements: course.unlock(), PageRequest: billing.PageRequest{Limit: billing.MaxBatchItems}})
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		products := []string{}
		for _, product := range offers.Items {
			products = append(products, product.Key)
		}
		slices.Sort(products)
		c.JSON(http.StatusPaymentRequired, gin.H{"error": "access_required", "products": products, "buy": "/courses/" + course.Slug + "/buy"})
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

// anyHeld reports whether owned holds any of keys.
func anyHeld(owned map[string]bool, keys []string) bool {
	return slices.ContainsFunc(keys, func(k string) bool { return owned[k] })
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
