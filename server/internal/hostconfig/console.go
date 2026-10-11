package hostconfig

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// ConsoleMount is where the standalone server serves its admin console.
type ConsoleMount struct {
	// Path is an absolute path without a trailing slash. Empty is "/admin".
	Path string
	// AuthBaseURL is the AuthKit JSON API; empty is the server's own.
	AuthBaseURL string
	// Extensions is data for the console extensions, keyed by extension id.
	Extensions map[string]any
}

// AdminConsolePath returns the console's configured path, defaulting to /admin.
func AdminConsolePath(c *ConsoleMount) string {
	if c == nil || c.Path == "" {
		return "/admin"
	}
	return c.Path
}

var consoleExtensionIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateConsoleExtensions checks extension ids and their JSON data.
func ValidateConsoleExtensions(name string, extensions map[string]any) error {
	for id, value := range extensions {
		if !consoleExtensionIDRe.MatchString(id) {
			return fmt.Errorf("invalid %s key %q: want an extension id of lowercase letters, digits and -", name, id)
		}
		if _, err := json.Marshal(value); err != nil {
			return fmt.Errorf("invalid %s.%s: %w", name, id, err)
		}
	}
	return nil
}
