package hostconfig

// ConsoleMount is where the standalone server serves its admin console.
type ConsoleMount struct {
	// Path is an absolute path without a trailing slash. Empty is "/admin".
	Path string
	// AuthBaseURL is the AuthKit JSON API; empty is the server's own.
	AuthBaseURL string
}

// AdminConsolePath returns the console's configured path, defaulting to /admin.
func AdminConsolePath(c *ConsoleMount) string {
	if c == nil || c.Path == "" {
		return "/admin"
	}
	return c.Path
}
