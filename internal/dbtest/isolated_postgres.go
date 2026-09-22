//go:build integration

package dbtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// IsolatedPostgresDSN starts a separate owned cluster for physical-identity
// negative controls. It never reuses the ordinary shared test database/server.
// Cleanup is registered even if a partially started container reports an error.
func IsolatedPostgresDSN(t *testing.T) string {
	t.Helper()
	server, err := testcontainers.GenericContainer(t.Context(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "postgres:18-alpine", ExposedPorts: []string{"5432/tcp"},
			Env:                map[string]string{"POSTGRES_USER": "test", "POSTGRES_PASSWORD": "test", "POSTGRES_DB": "postgres"},
			WaitingFor:         wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			HostConfigModifier: PostgresHostConfigModifier,
		}, Started: true,
	})
	if server != nil {
		t.Cleanup(func() { require.NoError(t, server.Terminate(context.Background())) })
	}
	require.NoError(t, err)
	host, err := server.Host(t.Context())
	require.NoError(t, err)
	port, err := server.MappedPort(t.Context(), "5432/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("postgres://test:test@%s:%s/postgres?sslmode=disable", host, port.Port())
}
