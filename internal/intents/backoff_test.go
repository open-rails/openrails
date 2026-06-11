package intents

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackoffPolicyProgressionAndCap(t *testing.T) {
	p := BackoffPolicy{Base: 2 * time.Minute, Cap: 2 * time.Hour}

	assert.Equal(t, 2*time.Minute, p.Delay(1))
	assert.Equal(t, 4*time.Minute, p.Delay(2))
	assert.Equal(t, 8*time.Minute, p.Delay(3))
	assert.Equal(t, 64*time.Minute, p.Delay(6))
	assert.Equal(t, 2*time.Hour, p.Delay(7), "doubles past the cap clamp to the cap")
	assert.Equal(t, 2*time.Hour, p.Delay(50), "stays capped without overflowing")
}

func TestBackoffPolicyDefensiveInputs(t *testing.T) {
	p := BackoffPolicy{Base: 2 * time.Minute, Cap: 2 * time.Hour}
	assert.Equal(t, p.Delay(1), p.Delay(0), "attempts < 1 behaves like the first attempt")
	assert.Equal(t, p.Delay(1), p.Delay(-3))

	zero := BackoffPolicy{}
	assert.Equal(t, time.Minute, zero.Delay(1), "zero policy falls back to sane defaults")
	assert.Equal(t, time.Hour, zero.Delay(30))
}
