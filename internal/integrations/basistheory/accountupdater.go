package basistheory

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Real-time Account Updater: one card token per call (Visa and Mastercard).
// Every NMI charge substitutes the token's own PAN, so this call is what keeps
// a Basis Theory card chargeable through a reissue.

// Account Updater result codes.
const (
	AUUpdatedPAN          = "UPD_PAN"
	AUUpdatedExpDate      = "UPD_EXP_DATE"
	AUNoUpdate            = "NO_UPDATE"
	AUNoMatch             = "NO_MATCH"
	AUClosedAccount       = "WRN_CLOSED_ACCOUNT"
	AUContactCardholder   = "WRN_CONTACT_CARDHOLDER"
	AUIssuerNotEnrolled   = "WRN_ISSUER_NOT_ENROLLED"
	AUIssuerNoData        = "WRN_ISSUER_NO_DATA"
	AUOptOut              = "WRN_OPT_OUT"
	AUUnsupportedNetwork  = "WRN_UNSUPPORTED_NETWORK"
	auUpdatedPrefix       = "UPD_"
	accountUpdaterRTRoute = "/account-updater/real-time"
)

// AUOutcome is what OpenRails may do about one result. The wire code is always
// recorded verbatim; this only maps it onto existing actions.
type AUOutcome string

const (
	// AUOutcomeUpdated: the network reissued the credential; adopt the new token.
	AUOutcomeUpdated AUOutcome = "updated"
	// AUOutcomeClosed: the account is closed. Never delete, never cancel.
	AUOutcomeClosed AUOutcome = "closed"
	// AUOutcomeContactCardholder: the network says nothing more without the cardholder.
	AUOutcomeContactCardholder AUOutcome = "contact_cardholder"
	// AUOutcomeNoChange: nothing was reported. No evidence, no action.
	AUOutcomeNoChange AUOutcome = "no_change"
	// AUOutcomeUnrecognized: an error or a code this build does not know.
	// Acted on by nothing: never guess on a money path.
	AUOutcomeUnrecognized AUOutcome = "unrecognized"
)

// ClassifyAccountUpdaterResult maps a verbatim result code onto its outcome.
// UPD_ matches by prefix: every update carries the new token.
func ClassifyAccountUpdaterResult(code string) AUOutcome {
	code = strings.ToUpper(strings.TrimSpace(code))
	switch code {
	case "", AUNoUpdate, AUNoMatch, AUIssuerNotEnrolled, AUIssuerNoData, AUOptOut, AUUnsupportedNetwork:
		return AUOutcomeNoChange
	case AUClosedAccount:
		return AUOutcomeClosed
	case AUContactCardholder:
		return AUOutcomeContactCardholder
	}
	if strings.HasPrefix(code, auUpdatedPrefix) {
		return AUOutcomeUpdated
	}
	return AUOutcomeUnrecognized
}

// CardUpdate is one real-time Account Updater answer. NewToken is set when an
// update was found; its id may be new, deduplicated or the original token.
type CardUpdate struct {
	NewToken   *CardToken `json:"new_token"`
	ResultCode string     `json:"result_code"`
}

// UpdateCard asks the networks for a card token's current card. The call is
// billed and may mint a token, so it carries an idempotency key: a repeat
// within 24 hours returns the first answer.
func (c *Client) UpdateCard(ctx context.Context, tokenID, idempotencyKey string) (*CardUpdate, error) {
	tokenID, idempotencyKey = strings.TrimSpace(tokenID), strings.TrimSpace(idempotencyKey)
	if tokenID == "" || idempotencyKey == "" {
		return nil, errors.New("basistheory: account updater needs a token id and an idempotency key")
	}
	var out CardUpdate
	if err := c.doJSON(ctx, http.MethodPost, accountUpdaterRTRoute, map[string]string{"token_id": tokenID}, idempotencyKey, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.ResultCode) == "" {
		return nil, errors.New("basistheory: account updater answered without a result code")
	}
	return &out, nil
}
