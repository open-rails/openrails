package hostconfig

import (
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Kubernetes names every Service in the pod's namespace in the environment
// (enableServiceLinks, on by default). A Service named redis or db lands in
// the redis or db section; these are its variables.
var (
	serviceHostVar = regexp.MustCompile(`^[A-Z0-9_]+_SERVICE_HOST$`)
	servicePortVar = regexp.MustCompile(`^[A-Z0-9_]+_SERVICE_PORT(_[A-Z0-9_]+)?$`)
	linkPortVar    = regexp.MustCompile(`^[A-Z0-9_]+_PORT$`)
	linkVar        = regexp.MustCompile(`^[A-Z0-9_]+_PORT_([0-9]+)_(TCP|UDP|SCTP)(_PROTO|_PORT|_ADDR)?$`)
)

// serviceLink reports whether name=value is a variable Kubernetes injects for
// a Service. Name and value must both match, so DB_PORT=5432 is still config
// while DB_PORT=tcp://10.96.0.12:5432 is a Service named db.
func serviceLink(name, value string) bool {
	if m := linkVar.FindStringSubmatch(name); m != nil {
		port, proto := m[1], strings.ToLower(m[2])
		switch m[3] {
		case "":
			hostPort, ok := strings.CutPrefix(value, proto+"://")
			_, p, _ := net.SplitHostPort(hostPort)
			return ok && isIPPort(hostPort) && p == port
		case "_PROTO":
			return value == proto
		case "_PORT":
			return value == port
		default:
			return net.ParseIP(value) != nil
		}
	}
	switch {
	case serviceHostVar.MatchString(name):
		return net.ParseIP(value) != nil
	case servicePortVar.MatchString(name):
		return isPort(value)
	case linkPortVar.MatchString(name):
		proto, hostPort, ok := strings.Cut(value, "://")
		return ok && (proto == "tcp" || proto == "udp" || proto == "sctp") && isIPPort(hostPort)
	}
	return false
}

func isPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535
}

func isIPPort(s string) bool {
	host, port, err := net.SplitHostPort(s)
	return err == nil && net.ParseIP(host) != nil && isPort(port)
}
