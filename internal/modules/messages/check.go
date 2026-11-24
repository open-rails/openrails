package messages

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
)

// languageTag is the shape of a BCP 47 tag: a language, then subtags.
var languageTag = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// Check validates a merchant's messages document: its languages are tags,
// each event type it names is one a message can be about, and every message
// renders against its event type's sample data for a customer of each
// language the document names. The error names the layout or event type.
func Check(s billing.MessageSettings) error {
	if s.DefaultLanguage != "" && !languageTag.MatchString(s.DefaultLanguage) {
		return fmt.Errorf("default_language: %q is not a language tag", s.DefaultLanguage)
	}
	langs := []string{""}
	named := func(lang string) {
		if !slices.Contains(langs, lang) {
			langs = append(langs, lang)
		}
	}
	receipt := Sample(string(models.NotificationOneOffPurchaseCompleted))
	for _, lang := range slices.Sorted(maps.Keys(s.Layouts)) {
		if !languageTag.MatchString(lang) {
			return fmt.Errorf("layouts.%s: not a language tag", lang)
		}
		_, layout, _, _ := resolve(s, receipt.EventType, lang)
		m, err := compile(billing.MessageTemplate{Subject: "-", Text: "-", HTML: "-"}, layout)
		if err == nil {
			_, err = m.render(receipt)
		}
		if err != nil {
			return fmt.Errorf("layouts.%s: %w", lang, err)
		}
		named(lang)
	}
	for _, kind := range slices.Sorted(maps.Keys(s.Events)) {
		if !Known(kind) {
			return fmt.Errorf("events.%s: not an event type a message can be about", kind)
		}
		for _, lang := range slices.Sorted(maps.Keys(s.Events[kind].Templates)) {
			if !languageTag.MatchString(lang) {
				return fmt.Errorf("events.%s.templates.%s: not a language tag", kind, lang)
			}
			named(lang)
		}
	}
	for _, lang := range langs {
		for _, kind := range EventTypes() {
			if _, err := Render(s, lang, Sample(kind)); err != nil {
				if lang == "" {
					return fmt.Errorf("events.%s: %w", kind, err)
				}
				return fmt.Errorf("events.%s (%s): %w", kind, lang, err)
			}
		}
	}
	return nil
}
