package middleware

import "strings"

// DefaultMaxBodyBytes is the default request body-size cap shared by the gin
// standalone middleware (ginmw.BodyLimit) and the gin-free embedded assembler
// (BodyLimitHTTP).
const DefaultMaxBodyBytes int64 = 1 << 20

func isDebugNMITokenizationPath(path string) bool {
	return strings.EqualFold(path, "/debug/nmi/tokenization")
}
