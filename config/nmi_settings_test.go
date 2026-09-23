package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNMIEndpointDeployment(t *testing.T) {
	got, err := NMIEndpointDeployment(nil)
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = NMIEndpointDeployment(map[string]any{"endpoint_deployment": "gateway"})
	require.NoError(t, err)
	require.Equal(t, NMIEndpointGateway, got)
	got, err = NMIEndpointDeployment(map[string]any{"endpoint_deployment": "sandbox"})
	require.NoError(t, err)
	require.Equal(t, NMIEndpointSandbox, got)
	_, err = NMIEndpointDeployment(map[string]any{"endpoint_deployment": "live"})
	require.Error(t, err)
	_, err = NMIEndpointDeployment(map[string]any{"endpoint_deployment": true})
	require.Error(t, err)
}
