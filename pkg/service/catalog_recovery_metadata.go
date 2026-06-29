package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
)

const openRailsRecoveryVersion = "1"

func productBenefitFingerprint(product *models.Product) string {
	if product == nil {
		return ""
	}
	type credit struct {
		Key         string `json:"key"`
		Unit        string `json:"unit,omitempty"`
		Amount      int64  `json:"amount"`
		ExpiryHours *int   `json:"expiry_hours,omitempty"`
		Cadence     string `json:"cadence,omitempty"`
	}
	payload := struct {
		Entitlements []string `json:"entitlements,omitempty"`
		Credits      []credit `json:"credits,omitempty"`
	}{}
	for key := range product.EntitlementsSpec {
		key = strings.TrimSpace(key)
		if key != "" {
			payload.Entitlements = append(payload.Entitlements, key)
		}
	}
	sort.Strings(payload.Entitlements)
	creditKeys := make([]string, 0, len(product.CreditsSpec))
	for key := range product.CreditsSpec {
		creditKeys = append(creditKeys, key)
	}
	sort.Strings(creditKeys)
	for _, key := range creditKeys {
		c := product.CreditsSpec[key]
		payload.Credits = append(payload.Credits, credit{
			Key:         key,
			Unit:        c.Unit,
			Amount:      c.Amount,
			ExpiryHours: c.ExpiryHours,
			Cadence:     string(c.Cadence),
		})
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
