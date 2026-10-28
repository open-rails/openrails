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

// ProposalsQuery filters the proposals list.
type ProposalsQuery struct {
	PSPID  string `form:"psp_id"`
	PullID string `form:"pull_id"`
	Kind   string `form:"kind"`
	Source string `form:"source"`
	Status string `form:"status"`
}

func proposalExecutor(r *httprequest.Request) (*proposals.Executor, bool) {
	if r.State == nil || r.State.DB == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "proposals unavailable")
		return nil, false
	}
	return proposals.New(r.State.DB, r.State.Config, r.State.Clock), true
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
	var q ProposalsQuery
	if !r.BindQuery(&q) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	ids, ok := listIDs(r, billing.ParseFindingID)
	if !ok {
		return
	}
	p := billing.ProposalListParams{PageRequest: page, IDs: ids, Kind: billing.ChangeKind(strings.TrimSpace(q.Kind)),
		Source: billing.ProposalSource(strings.TrimSpace(q.Source)), Status: billing.FindingStatus(strings.TrimSpace(q.Status))}
	if v := strings.TrimSpace(q.PSPID); v != "" {
		id, err := billing.ParsePSPID(v)
		if err != nil {
			r.APIError(api.Coded(billing.CodeInvalidParam, "psp_id is a PSP id").WithParam("psp_id"))
			return
		}
		p.PSPID = id
	}
	if v := strings.TrimSpace(q.PullID); v != "" {
		id, err := billing.ParsePSPImportID(v)
		if err != nil {
			r.APIError(api.Coded(billing.CodeInvalidParam, "pull_id is a PSP import id").WithParam("pull_id"))
			return
		}
		p.Pull = id
	}
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

// ApproveProposals handles POST /v1/merchant/proposals/approve: a person
// approves 1 to 100 proposals, each in its own transaction, under one
// approval run. The Idempotency-Key replays the first answer; the same key
// with another body is refused.
func ApproveProposals(r *httprequest.Request) {
	key := strings.TrimSpace(r.Header("Idempotency-Key"))
	if key == "" {
		r.ErrorCode("idempotency_key_required", "Idempotency-Key header is required")
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
	out, err := x.Approve(ctx, proposals.Request{IDs: params.IDs, Notes: params.Notes, Actor: personActor(r)})
	if err != nil {
		_ = claim.Fail(ctx, err)
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
