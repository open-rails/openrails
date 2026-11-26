package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails/server/internal/controlplane"
)

// devOperatorEmail is the operator `task dev` seeds, whose password is
// published in Taskfile.yaml.
const devOperatorEmail = "operator@localhost.test"

// refuseExposedDevOperator refuses to serve beyond loopback while the
// development operator exists: its database is a development one, wherever
// it was copied.
func refuseExposedDevOperator(ctx context.Context, cp *controlplane.ControlPlane, host string) error {
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	_, err := cp.Core().User(ctx, iam.UserByEmail(devOperatorEmail))
	switch {
	case errors.Is(err, iam.ErrUserNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("look up the development operator: %w", err)
	}
	return fmt.Errorf("refusing to listen on %s: this database holds the development operator %s, whose password is published, so it is a development database. Listen on loopback (host 127.0.0.1, the default) or use a database without that account", host, devOperatorEmail)
}
