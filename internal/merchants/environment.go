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

// environmentRefusal is why a PSP on rail in environment cannot serve its
// merchant: a sandbox merchant's PSPs move no money, a live one's real money,
// and a live PSP never talks to a fake provider. "" when it can.
func (s *Service) environmentRefusal(psp, rail, environment string, sandbox bool) string {
	if err := config.ValidatePSPMoneyClass(psp, rail, environment, sandbox); err != nil {
		return err.Error()
	}
	if config.LiveEnvironment(rail, environment) && len(s.fakeProviders) > 0 {
		return fmt.Sprintf("a live PSP never talks to a fake provider (%s)", strings.Join(s.fakeProviders, ", "))
	}
	return ""
}

// merchantClass is the money class of a merchant's custodians: test for a
// sandbox merchant, live otherwise.
func merchantClass(sandbox bool) string {
	if sandbox {
		return config.ClassTest
	}
	return config.ClassLive
}
