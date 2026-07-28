package handlers

import (
	"errors"
	"net/http"

	log "github.com/sirupsen/logrus"

	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/shared/redact"
)

// solanaClientError maps a Solana service error onto a client-safe (status,
// message) pair. A transport failure of the RPC chain becomes a GENERIC 502:
// its text is third-party-formatted and carries endpoint URLs, which carry the
// merchant's provider credential (#SEC-17) — the detail is logged server-side
// instead. Any other error keeps its domain message, with credential-bearing
// query parameters scrubbed as a second line of defence.
func solanaClientError(err error, status int) (int, string) {
	if errors.Is(err, solanarpc.ErrAllRPCEndpointsFailed) {
		log.WithField("detail", redact.Secrets(err.Error())).
			Warn("solana: RPC chain unavailable; returning a generic error to the client (#SEC-17)")
		return http.StatusBadGateway, "Solana RPC is temporarily unavailable; please retry"
	}
	return status, redact.Secrets(err.Error())
}
