package cardholdername

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanonicalPreservesFullInternationalName(t *testing.T) {
	assert.Equal(t, "María  José Carreño Quiñones", Canonical("  María  José Carreño Quiñones  ", "ignored", "legacy"))
	assert.Equal(t, "Ada Lovelace", Canonical("", " Ada ", " Lovelace "))
}

func TestPartsProjectsWithoutInventingSurname(t *testing.T) {
	first, last := Parts("李 小龍", "ignored", "legacy")
	assert.Equal(t, "李", first)
	assert.Equal(t, "小龍", last)

	first, last = Parts("Prince", "", "")
	assert.Equal(t, "Prince", first)
	assert.Empty(t, last)

	first, last = Parts("", "María de", "la Vega")
	assert.Equal(t, "María de", first, "legacy explicit parts remain unchanged")
	assert.Equal(t, "la Vega", last)
}
