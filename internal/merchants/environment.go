package merchants

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/config"
)

// WithFakeProviders names the fake providers this deployment wires (loopback
// gateways, test transports): beside any of them a live PSP is refused.
func (s *Service) WithFakeProviders(names ...string) *Service {
	if s != nil {
		s.fakeProviders = append(s.fakeProviders, names...)
	}
	return s
}

// environmentRefusal is why a PSP on rail in environment cannot serve: a
// live PSP never talks to a fake provider. "" when it can.
func (s *Service) environmentRefusal(rail, environment string) string {
	if config.LiveEnvironment(rail, environment) && len(s.fakeProviders) > 0 {
		return fmt.Sprintf("a live PSP never talks to a fake provider (%s)", strings.Join(s.fakeProviders, ", "))
	}
	return ""
}
