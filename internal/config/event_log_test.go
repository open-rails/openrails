package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEventRetention(t *testing.T) {
	for _, raw := range []string{"", " ", "2160h"} {
		got, err := EventRetentionDuration(&Config{EventRetention: raw})
		require.NoError(t, err)
		require.Equal(t, 90*24*time.Hour, got)
	}
	for _, raw := range []string{"0s", "-1h", "1ns", "bad"} {
		_, err := EventRetentionDuration(&Config{EventRetention: raw})
		require.Error(t, err)
	}
	got, err := EventRetentionDuration(&Config{EventRetention: "24h"})
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, got)
}
