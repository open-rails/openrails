package wireform

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Times leave in UTC and nil lists as []; a nil map stays null. The caller's
// value is untouched.
func TestWireForm(t *testing.T) {
	type inner struct {
		At   time.Time `json:"at"`
		Tags []string  `json:"tags"`
	}
	type body struct {
		ID     uuid.UUID         `json:"id"`
		At     *time.Time        `json:"at"`
		Items  []inner           `json:"items"`
		Labels map[string]string `json:"labels"`
		Any    any               `json:"any"`
		Raw    json.RawMessage   `json:"raw"`
		hidden time.Time
	}
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.FixedZone("x", -6*3600))
	in := body{ID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), At: &at, Items: []inner{{At: at}}, Any: map[string]any{"at": at}, hidden: at}
	out, err := json.Marshal(Of(in))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"11111111-1111-4111-8111-111111111111","at":"2026-10-04T18:00:00Z","items":[{"at":"2026-10-04T18:00:00Z","tags":[]}],"labels":null,"any":{"at":"2026-10-04T18:00:00Z"},"raw":null}`, string(out))
	require.Equal(t, at.Location(), in.At.Location(), "the caller's value is unchanged")
	require.Nil(t, Of(nil))
}
