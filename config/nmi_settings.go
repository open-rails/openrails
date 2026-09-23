package config

import "fmt"

const (
	NMIEndpointGateway = "gateway"
	NMIEndpointSandbox = "sandbox"
)

// NMIEndpointDeployment selects a gateway deployment, never credential posture.
// An omitted setting preserves historical test-mode endpoint selection.
func NMIEndpointDeployment(settings map[string]any) (string, error) {
	raw, exists := settings["endpoint_deployment"]
	if !exists {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok || value != NMIEndpointGateway && value != NMIEndpointSandbox {
		return "", fmt.Errorf("NMI endpoint_deployment must be gateway or sandbox")
	}
	return value, nil
}
