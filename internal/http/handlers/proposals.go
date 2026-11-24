package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/proposals"
)

func proposalExecutor(r *httprequest.Request) (*proposals.Executor, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "proposals unavailable")
		return nil, false
	}
	return proposals.New(r.State.DB, r.State.Config, r.State.Clock, r.State.DestructiveAdmissions), true
}

// personActor names the person deciding: the staff member, or the person the
// in-process host acts for (the host itself when it names none).
func personActor(r *httprequest.Request) string {
	if staff, ok := r.Staff(); ok {
		return billingauth.InvokerKey(staff.Identity)
	}
	return "unknown"
}

// ListProposals handles GET /v1/merchant/proposals: open pull.* proposals
// unless a status is named, most severe first.
func ListProposals(r *httprequest.Request) {
	var p billing.ProposalListParams
	if !bindList(r, &p, &p.PageRequest) {
		return
	}
	var ok bool
	if p.IDs, ok = listIDs(r, p.IDs); !ok {
		return
	}
	p.Kind = billing.ChangeKind(strings.TrimSpace(string(p.Kind)))
	p.Source = billing.ProposalSource(strings.TrimSpace(string(p.Source)))
	p.Status = billing.FindingStatus(strings.TrimSpace(string(p.Status)))
	x, ok := proposalExecutor(r)
	if !ok {
		return
	}
	out, err := x.List(r.Request.Context(), p)
	if err != nil {
		writeRefusal(r, err, "list proposals failed")
		return
	}
	r.SuccessJSON(out)
}

// approveOperation names approve calls in the idempotency store.
const approveOperation = "proposals.approve"

// ProposalChangeRoutes are the staff routes whose effect each change kind
// runs: approving a proposal needs what calling them needs, besides the
// approve route's merchant:config:manage. Every kind is listed; one missing
// here is refused. A kind that charges a customer or changes a
// subscription's price, billing or access names customer support's route. The
// kinds with none only record what the provider already did and billed: a
// subscription, charge, refund, renewal or card it holds, none of them a
// charge, price or access OpenRails decides.
var ProposalChangeRoutes = map[billing.ChangeKind][]string{
	billing.ChangeCreateSubscription:      nil,
	billing.ChangeRecordCharge:            nil,
	billing.ChangeRecordRefund:            nil,
	billing.ChangeAdoptSubscription:       nil,
	billing.ChangeUpdateCard:              nil,
	billing.ChangeCancelSubscription:      {"POST /v1/admin/subscriptions/{id}/cancel"},
	billing.ChangeRevokeAccess:            {"POST /v1/admin/product-access/{id}/revoke"},
	billing.ChangeRefundChargeAfterCancel: {"POST /v1/admin/payments/{id}/refunds"},
	billing.ChangeCancelProviderSchedule:  {"POST /v1/admin/subscriptions/{id}/cancel"},
	// Collecting a skipped renewal charges the customer; a correction moves
	// the price, schedule and start a subscription bills on, and can end a
	// product's access.
	billing.ChangeCollectSkippedRenewal: {"POST /v1/admin/subscriptions/{id}/change"},
	billing.ChangeCorrectSubscription:   {"POST /v1/admin/subscriptions/{id}/change"},
}

// changeAuthorizer asks can for each change kind's routes: a kind this
// version does not list is refused, a route's refusal is a staffCanRefusal.
func changeAuthorizer(r *httprequest.Request, can StaffCan) func(billing.ChangeKind) error {
	return func(kind billing.ChangeKind) error {
		keys, known := ProposalChangeRoutes[kind]
		if !known {
			return api.Coded(billing.CodeFindingNotActionable, "a proposal names a change this version does not authorize")
		}
		if err := staffCanAll(r, can, keys); err != nil {
			return staffCanRefusal{err}
		}
		return nil
	}
}

// staffCanRefusal is a proposal whose change's own route refused the caller.
type staffCanRefusal struct{ err error }

func (e staffCanRefusal) Error() string { return e.err.Error() }

// ApproveProposals handles POST /v1/merchant/proposals/approve: a person
// approves 1 to 100 proposals, each in its own transaction, under one
// approval run. The Idempotency-Key replays the first answer; the same key
// with another body is refused. A change that refunds, revokes or cancels
// also needs what its own route needs, asked of can before anything runs.
func ApproveProposals(can StaffCan) func(r *httprequest.Request) {
	return func(r *httprequest.Request) { approveProposals(r, can) }
}

func approveProposals(r *httprequest.Request, can StaffCan) {
	key := strings.TrimSpace(r.Header("Idempotency-Key"))
	if key == "" {
		r.ErrorCode(billing.CodeIdempotencyKeyRequired, "Idempotency-Key header is required")
		return
	}
	if len(key) > 200 {
		r.ErrorCode(billing.CodeInvalidParam, "Idempotency-Key is at most 200 bytes")
		return
	}
	var params billing.ApproveProposalsParams
	if !r.BindJSON(&params) {
		return
	}
	if len(params.IDs) == 0 || len(params.IDs) > billing.MaxBatchItems {
		r.APIError(api.Coded(billing.CodeInvalidParam, "ids names 1 to 100 proposals").WithParam("ids"))
		return
	}
	x, ok := proposalExecutor(r)
	if !ok {
		return
	}
	if r.State.Idempotency == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "proposals unavailable")
		return
	}
	body, err := json.Marshal(params)
	if err != nil {
		r.InternalError("encode approval", err)
		return
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	ctx := r.Request.Context()
	claim, rec, err := r.State.Idempotency.Begin(ctx, approveOperation, key)
	if err != nil {
		r.InternalError("claim idempotency key", err)
		return
	}
	if claim == nil {
		replayApproval(r, rec, digest)
		return
	}
	out, err := x.Approve(ctx, proposals.Request{IDs: params.IDs, Notes: params.Notes, Actor: personActor(r), Authorize: changeAuthorizer(r, can)})
	if err != nil {
		_ = claim.Fail(ctx, err)
		var refused staffCanRefusal
		if errors.As(err, &refused) {
			refuseStaffCan(r, refused.err)
			return
		}
		writeRefusal(r, err, "approve proposals failed")
		return
	}
	stored, err := json.Marshal(approvalReplay{Digest: digest, Results: out})
	if err == nil {
		err = claim.Complete(ctx, stored)
	}
	if err != nil && !errors.Is(err, idempotency.ErrClaimLost) {
		r.InternalError("settle idempotency key", err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// approvalReplay is what an approve key answers again.
type approvalReplay struct {
	Digest  string                  `json:"digest"`
	Results billing.ProposalResults `json:"results"`
}

func replayApproval(r *httprequest.Request, rec *idempotency.Record, digest string) {
	if rec == nil || rec.Status != idempotency.StatusSucceeded {
		r.ErrorCode(billing.CodeIdempotencyKeyInUse, "")
		return
	}
	var first approvalReplay
	if err := json.Unmarshal(rec.Result, &first); err != nil {
		r.InternalError("read idempotency key", err)
		return
	}
	if first.Digest != digest {
		r.ErrorCode(billing.CodeIdempotencyKeyReused, "the Idempotency-Key was first sent with other ids or notes")
		return
	}
	r.JSON(http.StatusOK, first.Results)
}

// DismissProposals handles POST /v1/merchant/proposals/dismiss: a person
// closes 1 to 100 proposals without applying them; notes are required.
func DismissProposals(r *httprequest.Request) {
	var params billing.DismissProposalsParams
	if !r.BindJSON(&params) {
		return
	}
	x, ok := proposalExecutor(r)
	if !ok {
		return
	}
	out, err := x.Dismiss(r.Request.Context(), proposals.Request{IDs: params.IDs, Notes: params.Notes, Actor: personActor(r)})
	if err != nil {
		writeRefusal(r, err, "dismiss proposals failed")
		return
	}
	r.JSON(http.StatusOK, out)
}
