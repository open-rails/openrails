package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"

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
	payload.Entitlements = slices.Clone(product.Entitlements)
	sort.Strings(payload.Entitlements)
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
