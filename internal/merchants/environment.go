package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
)

// WithFakeProviders names the fake providers this deployment wires (loopback
// gateways, test transports): beside any of them a live PSP is refused.
func (s *Service) WithFakeProviders(names ...string) *Service {
	if s != nil {
		s.fakeProviders = append(s.fakeProviders, names...)
	}
	return s
}

// environmentRefusal is why a PSP in environment cannot join this deployment,
// whose PSPs are all live or all non-live; "" when it can.
func (s *Service) environmentRefusal(ctx context.Context, q *gen.Queries, environment string) (string, error) {
	class := []string{config.EnvironmentTest, config.EnvironmentSandbox}
	if config.LiveEnvironment(environment) {
		if len(s.fakeProviders) > 0 {
			return fmt.Sprintf("a live PSP never talks to a fake provider (%s)", strings.Join(s.fakeProviders, ", ")), nil
		}
		class = []string{config.EnvironmentLive}
	}
	row, err := q.PSPOutsideEnvironments(ctx, class)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("a deployment's PSPs are all live or all non-live: %s's PSP %s (%s %s) is %s, this one %s",
		row.Slug, row.Key, row.Rail, row.AccountID, row.Environment, environment), nil
}

// merchantEnvironmentClass is the money class of a merchant's PSP documents,
// which its custodians record: live when they are, test otherwise.
func merchantEnvironmentClass(environments ...string) string {
	for _, environment := range environments {
		if config.LiveEnvironment(environment) {
			return config.EnvironmentLive
		}
	}
	return config.EnvironmentTest
}
