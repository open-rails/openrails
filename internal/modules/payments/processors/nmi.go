// Package processors provides helpers for identifying processor types and their underlying gateways.
package processors

import (
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// NMIBackedProcessors is the set of processors that use NMI as their underlying gateway.
// This is derived from config at startup. The key is the lowercase processor name.
var NMIBackedProcessors = map[string]bool{
	string(models.ProcessorMobius): true, // Default NMI-backed processor
}

// InitNMIBackedProcessors initializes the NMI-backed processors set from configuration.
// Call this at application startup after loading config.
func InitNMIBackedProcessors(cfg *config.Config) {
	if cfg == nil {
		return
	}

	// Clear and rebuild from config
	NMIBackedProcessors = make(map[string]bool)

	nmiProcessors := cfg.GetNMIProcessors()
	for name := range nmiProcessors {
		key := normalize.Lower(name)
		if key != "" {
			NMIBackedProcessors[key] = true
		}
	}

	// Ensure mobius is always included as a default if nothing configured
	if len(NMIBackedProcessors) == 0 {
		NMIBackedProcessors[string(models.ProcessorMobius)] = true
	}
}

// IsNMIBacked returns true if the given processor uses NMI as its underlying gateway.
// This is the ONLY place in the codebase that should know which processors use NMI.
func IsNMIBacked(processor string) bool {
	key := normalize.Lower(processor)
	return NMIBackedProcessors[key]
}

func IsConfigured(cfg *config.Config, processor string) bool {
	if cfg == nil {
		return false
	}
	return cfg.GetProcessorType(normalize.Lower(processor)) != ""
}

// IsNMIBackedProcessor returns true if the given models.Processor uses NMI as its gateway.
func IsNMIBackedProcessor(processor models.Processor) bool {
	return IsNMIBacked(string(processor))
}

// GetNMIBackedProcessorsList returns a slice of all NMI-backed processor names.
// Useful for database queries that need to filter by NMI-backed processors.
func GetNMIBackedProcessorsList() []string {
	result := make([]string, 0, len(NMIBackedProcessors))
	for name := range NMIBackedProcessors {
		result = append(result, name)
	}
	return result
}
