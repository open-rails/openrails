package history

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/open-rails/openrails/billing"
)

func TestAnOversizedOperationEntryIsCutNotDropped(t *testing.T) {
	huge := strings.Repeat("é", MaxOperationPayload)
	op := Operation{Version: Version, Kind: "nmi_vault_delete", Attempt: 2, Reason: huge, Evidence: map[string]any{"body": huge}}
	actor := Staff(strings.Repeat("ü", MaxActorBytes))
	fitted, short, err := op.Fit(actor)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(fitted)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxOperationPayload || !fitted.Truncated || fitted.Evidence != nil {
		t.Fatalf("the entry fits, says it was cut and keeps no evidence map: %d bytes, %+v", len(raw), fitted.Truncated)
	}
	if utf8.RuneCountInString(fitted.Reason) != MaxOperationReason || !strings.HasPrefix(huge, fitted.Reason) {
		t.Fatalf("the reason keeps its first %d characters", MaxOperationReason)
	}
	if utf8.RuneCountInString(fitted.EvidenceExcerpt) != MaxOperationEvidence || !strings.HasPrefix(fitted.EvidenceExcerpt, `{"body":"é`) {
		t.Fatalf("the evidence keeps the start of its JSON: %.40q", fitted.EvidenceExcerpt)
	}
	if utf8.RuneCountInString(string(short)) != MaxActorRunes || len(short) > MaxActorBytes || !strings.HasPrefix(string(short), "staff:ü") {
		t.Fatalf("an actor over the log's bytes keeps its first characters: %d bytes", len(short))
	}

	small := Operation{Version: Version, Kind: "stripe_charge", Attempt: 1, Reason: "declined", Evidence: map[string]any{"code": "card_declined"}}
	same, kept, err := small.Fit(billing.LogActorSystem)
	if err != nil || same.Truncated || same.Reason != small.Reason || same.Evidence["code"] != "card_declined" || kept != billing.LogActorSystem {
		t.Fatalf("an entry within the limits is kept whole: %+v %s %v", same, kept, err)
	}
}
