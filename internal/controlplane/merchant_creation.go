package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrMerchantSlugReserved is the typed refusal for a user-claimed slug on the
// deployment's reserved list (merchant.ReservedHostedSlugs +
// MerchantCreationConfig.ReservedSlugs). Reserved slugs stay claimable
// through operator paths (Bootstrap, manifests, ownerless provisioning) —
// authkit's route additionally allows its ReservedEscalationRole, which the
// in-process check does not evaluate.
var ErrMerchantSlugReserved = errors.New("controlplane: merchant slug is reserved")

// ErrMerchantCreationRefused is the typed refusal from the deployment's
// admission (cost) gate — the ak#263 WithInstanceAdmission seam.
var ErrMerchantCreationRefused = errors.New("controlplane: merchant creation refused")

// EnforceMerchantCreationPolicy applies the deployment's declared merchant
// creation policy (or#914, WithMerchantCreation) to an IN-PROCESS user-claimed
// slug: the reserved-slug list, the extra slug pattern, and the host admission
// gate — the same config authkit enforces on its generated POST /merchant
// route (which additionally rate-limits per IP/user and can escalate reserved
// slugs). No-op when creation is not enabled or ownerUserID is empty (operator
// paths are not user claims).
func (c *ControlPlane) EnforceMerchantCreationPolicy(ctx context.Context, slug, ownerUserID string) error {
	if c == nil || c.merchantCreation == nil || strings.TrimSpace(ownerUserID) == "" {
		return nil
	}
	slug = merchant.NormalizeSlug(slug)
	for _, r := range merchant.ReservedHostedSlugs {
		if slug == r {
			return fmt.Errorf("%w: %q", ErrMerchantSlugReserved, slug)
		}
	}
	for _, r := range c.merchantCreation.ReservedSlugs {
		if slug == strings.ToLower(strings.TrimSpace(r)) {
			return fmt.Errorf("%w: %q", ErrMerchantSlugReserved, slug)
		}
	}
	if c.merchantCreationPattern != nil && !c.merchantCreationPattern.MatchString(slug) {
		return fmt.Errorf("%w: slug %q does not match the deployment's creation pattern", ErrMerchantSlugReserved, slug)
	}
	if admit := c.merchantCreation.Admission; admit != nil {
		if err := admit(ctx, slug, strings.TrimSpace(ownerUserID)); err != nil {
			return fmt.Errorf("%w: %w", ErrMerchantCreationRefused, err)
		}
	}
	return nil
}

// merchantCreationAttachHandler wraps authkit's generated POST /merchant
// (ak#263): on a successful creation (or idempotent member re-run) it ensures
// the openrails.merchants directory row bound to the reported group, so ONE
// call both claims the name — behind authkit's velocity limits, reserved-slug
// escalation and the admission seam — and provisions the billing bucket
// (or#914 "registration is provisioning"). A row-attach failure surfaces as a
// 500: the group exists, and re-POSTing the same slug is the documented,
// idempotent repair.
func (c *ControlPlane) merchantCreationAttachHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &bufferedResponse{header: http.Header{}}
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusOK || rec.status == http.StatusCreated {
			var body struct {
				GroupID      string `json:"group_id"`
				InstanceSlug string `json:"instance_slug"`
			}
			if err := json.Unmarshal(rec.body.Bytes(), &body); err == nil &&
				strings.TrimSpace(body.GroupID) != "" && strings.TrimSpace(body.InstanceSlug) != "" {
				if err := c.attachMerchantDirectoryRow(r.Context(), body.InstanceSlug, body.GroupID); err != nil {
					log.WithError(err).WithField("merchant", body.InstanceSlug).
						Error("controlplane: merchant directory attach after generated creation failed (or#914); re-POST the same slug to repair")
					http.Error(w, "merchant provisioning failed", http.StatusInternalServerError)
					return
				}
			}
		}
		rec.flushTo(w)
	})
}

func (c *ControlPlane) attachMerchantDirectoryRow(ctx context.Context, slug, groupID string) error {
	dir, err := merchants.NewDirectoryService(c.pool)
	if err != nil {
		return err
	}
	_, _, err = dir.Provision(ctx, merchants.ProvisionRequest{Slug: slug, PermissionGroupID: groupID})
	return err
}

// bufferedResponse captures a handler's response so a post-success hook can
// run before anything reaches the client. The generated creation route writes
// one small JSON object; nothing here streams.
type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) flushTo(w http.ResponseWriter) {
	for k, vs := range b.header {
		for _, v := range vs {
			w.Header()[k] = append(w.Header()[k], v)
		}
	}
	if b.status != 0 {
		w.WriteHeader(b.status)
	}
	_, _ = w.Write(b.body.Bytes())
}
