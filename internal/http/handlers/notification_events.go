package handlers

import (
	"crypto/sha256"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/notificationevents"
	"github.com/open-rails/openrails/internal/requestauth"
)

// NotificationEventsQuery bounds one pending delivery batch.
type NotificationEventsQuery struct {
	Limit int `form:"limit"`
}

func notificationConsumer(r *httprequest.Request) (*notificationevents.Consumer, billing.MerchantID, bool) {
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		writeRefusal(r, err, "merchant required")
		return nil, mid, false
	}
	owner := "host:merchant"
	if _, host := requestauth.HostPrincipalFromContext(ctx); !host {
		who, ok := billingauth.IdentityFromContext(ctx)
		if !ok || who.SubjectKind != billingauth.SubjectApplication || !billingauth.SelfActing(who) {
			r.ErrorCode(billing.CodeApplicationRequired, "")
			return nil, mid, false
		}
		owner = fmt.Sprintf("app:%x", sha256.Sum256([]byte(who.Issuer+"\x00"+who.Subject)))
	}
	id := mid.UUID()
	c, err := notificationevents.New(r.State.DB, &id, owner, r.Param("name"))
	if err != nil {
		writeRefusal(r, err, "invalid notification consumer")
		return nil, mid, false
	}
	return c, mid, true
}
func RegisterNotificationConsumer(r *httprequest.Request) {
	c, _, ok := notificationConsumer(r)
	if !ok {
		return
	}
	if err := c.Register(r.Request.Context()); err != nil {
		writeRefusal(r, err, "register notification consumer failed")
		return
	}
	r.NoContent()
}
func PendingNotificationEvents(r *httprequest.Request) {
	c, _, ok := notificationConsumer(r)
	if !ok {
		return
	}
	var q NotificationEventsQuery
	if !r.BindQuery(&q) {
		return
	}
	events, err := c.Pending(r.Request.Context(), q.Limit)
	if err != nil {
		writeRefusal(r, err, "list notification events failed")
		return
	}
	r.SuccessJSON(billing.NotificationEvents{Data: events})
}
func AcknowledgeNotificationEvents(r *httprequest.Request) {
	c, mid, ok := notificationConsumer(r)
	if !ok {
		return
	}
	var body billing.AcknowledgeNotificationEventsParams
	if !r.BindJSON(&body) {
		return
	}
	ids, ok := batchIDs(r, body.NotificationIDs, 100, "notification_ids")
	if !ok {
		return
	}
	refs := make([]billing.NotificationEventRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, billing.NotificationEventRef{MerchantID: mid, ID: id})
	}
	if err := c.Acknowledge(r.Request.Context(), refs); err != nil {
		writeRefusal(r, err, "acknowledge notification events failed")
		return
	}
	r.NoContent()
}
func RetireNotificationConsumer(r *httprequest.Request) {
	c, _, ok := notificationConsumer(r)
	if !ok {
		return
	}
	if err := c.Retire(r.Request.Context()); err != nil {
		writeRefusal(r, err, "retire notification consumer failed")
		return
	}
	r.NoContent()
}
