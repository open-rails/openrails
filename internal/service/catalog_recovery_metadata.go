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
	payload := struct {
		Entitlements []string `json:"entitlements,omitempty"`
	}{}
	for key := range product.EntitlementsSpec {
		key = strings.TrimSpace(key)
		if key != "" {
			payload.Entitlements = append(payload.Entitlements, key)
		}
	}
	sort.Strings(payload.Entitlements)
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
