package intents

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeMode implements ModeView for the gate matrix.
type fakeMode struct {
	readonly bool
	limited  bool
}

func (m fakeMode) IsProviderReadOnly() bool { return m.readonly }
func (m fakeMode) IsLimitedMode() bool      { return m.limited || m.readonly }

func modeFull() fakeMode     { return fakeMode{} }
func modeLimited() fakeMode  { return fakeMode{limited: true} }
func modeReadonly() fakeMode { return fakeMode{readonly: true} }

// TestGateExecutionMatrix pins the full origin x mode gating matrix:
// user/admin-origin intents are reactive completions and execute under
// limited; system-origin requires full; NOTHING executes under readonly.
func TestGateExecutionMatrix(t *testing.T) {
	cases := []struct {
		name    string
		mode    ModeView
		origin  Origin
		blocked bool
	}{
		{"user/full executes", modeFull(), OriginUser, false},
		{"admin/full executes", modeFull(), OriginAdmin, false},
		{"system/full executes", modeFull(), OriginSystem, false},

		{"user/limited executes (reactive completion)", modeLimited(), OriginUser, false},
		{"admin/limited executes (reactive completion)", modeLimited(), OriginAdmin, false},
		{"system/limited parks (proactive)", modeLimited(), OriginSystem, true},

		{"user/readonly parks", modeReadonly(), OriginUser, true},
		{"admin/readonly parks", modeReadonly(), OriginAdmin, true},
		{"system/readonly parks", modeReadonly(), OriginSystem, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, reason := GateExecution(tc.mode, tc.origin)
			assert.Equal(t, tc.blocked, blocked)
			if tc.blocked {
				assert.NotEmpty(t, reason, "a park must always carry its reason")
			} else {
				assert.Empty(t, reason)
			}
		})
	}
}

func TestGateExecutionUnknownOriginParks(t *testing.T) {
	blocked, reason := GateExecution(modeFull(), Origin("robot"))
	assert.True(t, blocked, "unknown origins must park, never guess")
	assert.Contains(t, reason, "robot")
}

func TestGateExecutionNilConfigExecutes(t *testing.T) {
	blocked, _ := GateExecution(nil, OriginSystem)
	assert.False(t, blocked, "nil mode view (tests/dev) behaves as full")
}
