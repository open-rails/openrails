package handlers

import (
	"errors"
	"log/slog"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/shared/redact"
)

// solanaUnavailable is the refusal for a transient Solana outage, or nil. An
// RPC failure's text carries endpoint URLs, which carry the merchant's
// provider credential: it is logged, never answered.
func solanaUnavailable(logger *slog.Logger, err error) *api.APIError {
	switch {
	case errors.Is(err, vault.ErrUnavailable):
		return api.Coded(billing.CodeServiceUnavailable, "Solana signer is temporarily unavailable; please retry")
	case errors.Is(err, solanarpc.ErrAllRPCEndpointsFailed):
		logger.Warn("solana: RPC chain unavailable; returning a generic error to the client (#SEC-17)", "detail", redact.Secrets(err.Error()))
		return api.Coded(billing.CodeSolanaRPCUnavailable, "Solana RPC is temporarily unavailable; please retry")
	}
	return nil
}

// solanaClientError answers a wallet step (prepare or confirm) that failed:
// a transient outage, or solana_transaction_refused with the domain message,
// credential-bearing query parameters scrubbed as a second line of defence.
func solanaClientError(logger *slog.Logger, err error) *api.APIError {
	if refusal := solanaUnavailable(logger, err); refusal != nil {
		return refusal
	}
	return api.Coded(billing.CodeSolanaTransactionRefused, redact.Secrets(err.Error()))
}
