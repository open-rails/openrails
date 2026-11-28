package app

import (
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/retention"
)

// The intent types retention deletes are one list: the cleanup queries name
// exactly retention.OutboxIntentTypes, the partial index they read covers it,
// and every type on it is one a handler runs.
func TestOutboxIntentTypesAreOneList(t *testing.T) {
	want := slices.Sorted(slices.Values(retention.OutboxIntentTypes))
	registry := (&Runtime{Log: slog.Default(), DB: &db.DB{Log: slog.Default()}, PurchaseService: &payment.PurchaseService{}, CollectionResolver: &money.MerchantCollectionAdapterBuilder{}}).buildIntentRegistry(clockwork.NewFakeClock())
	registered := registry.Types()
	for _, typ := range want {
		if !slices.Contains(registered, typ) {
			t.Errorf("retention.OutboxIntentTypes names %q, which no handler runs", typ)
		}
	}

	queries, err := os.ReadFile("../db/queries/retention.sql")
	if err != nil {
		t.Fatal(err)
	}
	lists := regexp.MustCompile(`intent_type IN \(([^)]*)\)`).FindAllStringSubmatch(string(queries), -1)
	if len(lists) != 2 {
		t.Fatalf("retention.sql: want the merchant discovery and the delete lists, found %d", len(lists))
	}
	for _, list := range lists {
		if got := quoted(list[1]); !slices.Equal(got, want) {
			t.Errorf("retention.sql lists %v, retention.OutboxIntentTypes %v", got, want)
		}
	}

	schema, err := os.ReadFile("../migrate/postgres/0001_schema.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	index := regexp.MustCompile(`CREATE INDEX provider_intents_updated_at_idx[^;]*intent_type IN \(([^)]*)\)`).FindStringSubmatch(string(schema))
	if index == nil {
		t.Fatal("provider_intents_updated_at_idx not found")
	}
	covered := quoted(index[1])
	for _, typ := range want {
		if !slices.Contains(covered, typ) {
			t.Errorf("provider_intents_updated_at_idx does not cover %q: the cleanup could not use it", typ)
		}
	}
}

func quoted(list string) []string {
	var out []string
	for _, item := range strings.Split(list, ",") {
		out = append(out, strings.Trim(strings.TrimSpace(item), "'"))
	}
	return slices.Sorted(slices.Values(out))
}
