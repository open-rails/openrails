package request

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreferredLanguage(t *testing.T) {
	for header, want := range map[string]string{
		"pt-BR,pt;q=0.9":     "pt-BR",
		"de;q=0.5, fr;q=0.8": "fr",
		"zh-Hant-TW":         "zh-TW",
		"es-419":             "es-419",
		"en":                 "en",
		"*":                  "",
		"":                   "",
		"not a tag!":         "",
	} {
		require.Equal(t, want, PreferredLanguage(header), header)
	}
}
