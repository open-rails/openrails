package handlers

import (
	"errors"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/shared/redact"
)

const (
	codeSolanaTransactionRefused = "solana_transaction_refused"
	codeSolanaRPCUnavailable     = "solana_rpc_unavailable"
)

// solanaUnavailable is the refusal for a transient Solana outage, or nil. An
// RPC failure's text is third-party-formatted and carries endpoint URLs, which
// carry the merchant's provider credential (#SEC-17): it is logged, never
// answered.
func solanaUnavailable(err error) *api.APIError {
	switch {
	case errors.Is(err, vault.ErrUnavailable):
		return api.Coded(billing.CodeServiceUnavailable, "Solana signer is temporarily unavailable; please retry")
	case errors.Is(err, solanarpc.ErrAllRPCEndpointsFailed):
		log.WithField("detail", redact.Secrets(err.Error())).
			Warn("solana: RPC chain unavailable; returning a generic error to the client (#SEC-17)")
		return api.Coded(codeSolanaRPCUnavailable, "Solana RPC is temporarily unavailable; please retry")
	}
	return nil
}

// solanaClientError answers a wallet step (prepare or confirm) that failed:
// a transient outage, or solana_transaction_refused with the domain message,
// credential-bearing query parameters scrubbed as a second line of defence.
func solanaClientError(err error) *api.APIError {
	if refusal := solanaUnavailable(err); refusal != nil {
		return refusal
	}
	return api.Coded(codeSolanaTransactionRefused, redact.Secrets(err.Error()))
}
