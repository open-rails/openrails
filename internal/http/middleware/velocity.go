package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

// Velocity is a fixed-window budget of requests per client IP and per user.
type Velocity struct {
	Bucket string
	Limit  int
	Window time.Duration
}

// MerchantCreationVelocity caps merchant creation: a creation claims a name.
var MerchantCreationVelocity = Velocity{Bucket: "merchant-create", Limit: 12, Window: 24 * time.Hour}

// VelocityLimit enforces v per resolved client IP and per authenticated user;
// mount it after authentication. Redis shares the windows across replicas; the
// in-process store covers a missing or failing Redis.
func VelocityLimit(v Velocity, rdb *redis.Client, resolver *iputil.TrustedProxies) router.Middleware {
	store := NewRateLimitStore()
	return func(next router.Handler) router.Handler {
		return func(r *request.Request) {
			for _, subject := range rateLimitSubjectsHTTP(r.Request, resolver) {
				res := windowAllow(r.Request.Context(), rdb, store, subject.Key, v)
				if res.allowed {
					continue
				}
				log.WithFields(log.Fields{"bucket": v.Bucket, "limited_subject": subject.Key}).Warn("Rate limit exceeded")
				r.SetHeader("Retry-After", fmt.Sprintf("%d", int(math.Max(1, math.Ceil(res.reset.Seconds())))))
				r.APIError(&api.APIError{HTTPStatus: http.StatusTooManyRequests, Type: api.ErrorTypeRateLimit,
					Code: api.CodeRateLimitExceeded, Message: "rate limit exceeded"})
				return
			}
			next(r)
		}
	}
}

func windowAllow(ctx context.Context, rdb *redis.Client, store *RateLimitStore, subjectKey string, v Velocity) rateLimitResult {
	if rdb != nil {
		res, err := redisWindowAllow(ctx, rdb, subjectKey, v.Bucket, v.Limit, v.Window)
		if err == nil {
			return res
		}
		log.WithError(err).WithField("subject", subjectKey).Warn("Rate limit redis error; falling back to in-memory limiter")
	}
	return store.allowWindow(subjectKey, v.Bucket, v.Limit, v.Window)
}
