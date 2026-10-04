package config

import (
	"fmt"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

const (
	NMIEndpointGateway = "gateway"
	NMIEndpointSandbox = "sandbox"
)

// NMIEndpointDeployment selects a gateway deployment, never credential posture.
// An omitted setting preserves historical test-mode endpoint selection.
func NMIEndpointDeployment(settings map[string]any) (string, error) {
	raw, exists := settings["endpoint_deployment"]
	if !exists {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok || value != NMIEndpointGateway && value != NMIEndpointSandbox {
		return "", fmt.Errorf("NMI endpoint_deployment must be gateway or sandbox")
	}
	return value, nil
}

// A PSP's card_entry setting: where a new card is typed (#1129).
const (
	// CardEntryBrowser: the gateway's own fields take the card and OpenRails
	// receives a token. The default.
	CardEntryBrowser = "browser"
	// CardEntryServer: the page posts the card to OpenRails, which vaults it.
	// The server is in PCI scope (SAQ D), so it is an explicit declaration.
	CardEntryServer = "server"
)

// CardEntry reads a PSP's card_entry setting. server is refused on a rail with
// no server-side vault call and on a PSP whose cards a custodian holds.
func CardEntry(rail string, settings map[string]any, custodial bool) (string, error) {
	raw, declared := settings["card_entry"]
	if !declared {
		return CardEntryBrowser, nil
	}
	switch value, _ := raw.(string); value {
	case CardEntryBrowser:
		return CardEntryBrowser, nil
	case CardEntryServer:
		if !rails.SupportsServerCardEntry(models.Rail(rail)) {
			return "", fmt.Errorf("card_entry: server is not available on rail %q: it has no server-side vault call", rail)
		}
		if custodial {
			return "", fmt.Errorf("card_entry: server cannot be combined with a custodian: the custodian's page takes the card")
		}
		return CardEntryServer, nil
	}
	return "", fmt.Errorf("card_entry must be %s or %s", CardEntryBrowser, CardEntryServer)
}
