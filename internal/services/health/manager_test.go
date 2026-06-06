package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeChecker struct {
	name string
	err  error
}

func (f *fakeChecker) Name() string           { return f.name }
func (f *fakeChecker) Timeout() time.Duration { return 10 * time.Millisecond }
func (f *fakeChecker) Check(context.Context) error {
	return f.err
}

func TestServiceHealthManagerCircuitBreaker(t *testing.T) {
	manager := NewServiceHealthManager()
	manager.checkInterval = time.Hour
	manager.recoveryTimeout = time.Millisecond
	checker := &fakeChecker{name: "postgres"}
	manager.RegisterChecker(checker)

	manager.checkAllServices()
	require.True(t, manager.IsAvailable("postgres"))

	checker.err = errors.New("connection refused")
	for range manager.failureThreshold {
		manager.checkAllServices()
	}

	status, ok := manager.GetServiceHealth("postgres")
	require.True(t, ok)
	require.False(t, manager.IsAvailable("postgres"))
	require.True(t, status.CircuitOpen)
	require.Equal(t, manager.failureThreshold, status.ConsecutiveFailures)
	require.EqualValues(t, manager.failureThreshold, status.TotalFailures)

	checker.err = nil
	manager.checkAllServices()

	status, ok = manager.GetServiceHealth("postgres")
	require.True(t, ok)
	require.True(t, manager.IsAvailable("postgres"))
	require.False(t, status.CircuitOpen)
	require.Zero(t, status.ConsecutiveFailures)
	require.Nil(t, status.LastError)
}

func TestPeriodicChecksIncludeAvailableServices(t *testing.T) {
	manager := NewServiceHealthManager()
	checker := &fakeChecker{name: "postgres"}
	manager.RegisterChecker(checker)

	manager.checkAllServices()
	status, ok := manager.GetServiceHealth("postgres")
	require.True(t, ok)
	require.True(t, status.Available)
	initialChecks := status.TotalChecks

	checker.err = errors.New("connection refused")
	manager.checkServicesNeedingCheck()

	status, ok = manager.GetServiceHealth("postgres")
	require.True(t, ok)
	require.Equal(t, initialChecks+1, status.TotalChecks)
	require.Equal(t, 1, status.ConsecutiveFailures)
}
