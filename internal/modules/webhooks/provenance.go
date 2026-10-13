package webhooks

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/booktag"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// quietUnknownObject reports whether a webhook about a provider object no
// record names is informational: the book that created it (carried, "" for
// none) is another, or the PSP moves no money and the object predates the tag.
// The webhook then answers 200 and is only logged; an object this book
// created and lost keeps failing, so the provider redelivers it.
func quietUnknownObject(ctx context.Context, database *db.DB, carried string, subject string) bool {
	mid, ok := merchant.FromContext(ctx)
	psp := db.PSPIDFromContext(ctx)
	if !ok || psp == uuid.Nil || database == nil {
		return false
	}
	psps, err := database.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: psp})
	if err != nil {
		return false
	}
	tag, ok := booktag.Ensure(mid.UUID(), database.GenDirectory()).Tag(ctx)
	if !ok {
		return false
	}
	provenance := tag.Classify(carried, time.Time{})
	if !provenance.Quiet(psps.Live) {
		return false
	}
	log.WithContext(ctx).WithFields(log.Fields{"psp_id": psp.String(), "provenance": string(provenance), "object": subject}).
		Info("webhook names a provider object no record of this merchant names and this book did not create; answered quietly")
	return true
}

// nmiBookID reads the book id from a webhook's merchant-defined fields, which
// NMI sends as an object by field id; anything else reads as none.
func nmiBookID(raw json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	value, _ := fields[booktag.NMIFieldID].(string)
	return strings.TrimSpace(value)
}
