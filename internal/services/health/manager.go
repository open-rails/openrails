package health

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultCheckInterval    = 60 * time.Second
	defaultFailureThreshold = 3
	defaultRecoveryTimeout  = 60 * time.Second
)

// HealthChecker checks one external service dependency.
type HealthChecker interface {
	Name() string
	Timeout() time.Duration
	Check(ctx context.Context) error
}

// ServiceHealth tracks circuit-breaker state for one dependency.
type ServiceHealth struct {
	Name                string
	Available           bool
	LastCheck           time.Time
	LastSuccess         time.Time
	LastError           error
	ConsecutiveFailures int
	CircuitOpen         bool
	NextRetryAt         time.Time
	TotalChecks         int64
	TotalFailures       int64
}

// ServiceHealthManager monitors external dependencies and opens circuits after
// repeated failures.
type ServiceHealthManager struct {
	services map[string]*ServiceHealth
	checkers map[string]HealthChecker

	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	startOnce sync.Once
	stopOnce  sync.Once

	checkInterval    time.Duration
	failureThreshold int
	recoveryTimeout  time.Duration
}

// NewServiceHealthManager creates a manager with production defaults.
func NewServiceHealthManager() *ServiceHealthManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &ServiceHealthManager{
		services:         make(map[string]*ServiceHealth),
		checkers:         make(map[string]HealthChecker),
		ctx:              ctx,
		cancel:           cancel,
		checkInterval:    defaultCheckInterval,
		failureThreshold: defaultFailureThreshold,
		recoveryTimeout:  defaultRecoveryTimeout,
	}
}

// RegisterChecker registers a dependency checker.
func (m *ServiceHealthManager) RegisterChecker(checker HealthChecker) {
	if m == nil || checker == nil || checker.Name() == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	name := checker.Name()
	m.checkers[name] = checker
	if _, exists := m.services[name]; !exists {
		m.services[name] = &ServiceHealth{
			Name:        name,
			Available:   false,
			NextRetryAt: time.Now(),
		}
	}

	log.WithField("service", name).Info("registered health checker")
}

// Start begins background dependency checks.
func (m *ServiceHealthManager) Start() {
	if m == nil {
		return
	}
	m.startOnce.Do(func() {
		m.checkAllServices()
		go m.runPeriodicChecks()
		log.WithField("interval", m.checkInterval).Info("service health manager started")
	})
}

// Stop stops background dependency checks.
func (m *ServiceHealthManager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		m.cancel()
		log.Info("service health manager stopped")
	})
}

// IsAvailable reports whether a dependency is currently usable.
func (m *ServiceHealthManager) IsAvailable(serviceName string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	service, exists := m.services[serviceName]
	return exists && service.Available && !service.CircuitOpen
}

// GetServiceHealth returns a snapshot for one dependency.
func (m *ServiceHealthManager) GetServiceHealth(serviceName string) (*ServiceHealth, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	service, exists := m.services[serviceName]
	if !exists {
		return nil, false
	}
	return cloneServiceHealth(service), true
}

func (m *ServiceHealthManager) runPeriodicChecks() {
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.checkServicesNeedingCheck()
		}
	}
}

func (m *ServiceHealthManager) checkAllServices() {
	m.mu.RLock()
	checkers := make(map[string]HealthChecker, len(m.checkers))
	for name, checker := range m.checkers {
		checkers[name] = checker
	}
	m.mu.RUnlock()
	m.checkServices(checkers)
}

func (m *ServiceHealthManager) checkServicesNeedingCheck() {
	now := time.Now()

	m.mu.RLock()
	checkers := make(map[string]HealthChecker)
	for name, service := range m.services {
		if !service.CircuitOpen || !service.NextRetryAt.After(now) {
			if checker, exists := m.checkers[name]; exists {
				checkers[name] = checker
			}
		}
	}
	m.mu.RUnlock()
	m.checkServices(checkers)
}

func (m *ServiceHealthManager) checkServices(checkers map[string]HealthChecker) {
	var wg sync.WaitGroup
	for name, checker := range checkers {
		wg.Add(1)
		go func(serviceName string, hc HealthChecker) {
			defer wg.Done()
			m.checkService(serviceName, hc)
		}(name, checker)
	}
	wg.Wait()
}

func (m *ServiceHealthManager) checkService(serviceName string, checker HealthChecker) {
	checkCtx, cancel := context.WithTimeout(m.ctx, checker.Timeout())
	err := checker.Check(checkCtx)
	cancel()

	m.mu.Lock()
	defer m.mu.Unlock()

	service, exists := m.services[serviceName]
	if !exists {
		return
	}
	service.LastCheck = time.Now()
	service.TotalChecks++

	if err == nil {
		wasUnavailable := !service.Available || service.CircuitOpen
		service.Available = true
		service.CircuitOpen = false
		service.ConsecutiveFailures = 0
		service.LastSuccess = time.Now()
		service.LastError = nil
		service.NextRetryAt = time.Time{}
		if wasUnavailable {
			log.WithField("service", serviceName).Info("service became available")
		}
		return
	}

	wasAvailable := service.Available && !service.CircuitOpen
	m.recordFailureLocked(service, err)
	if wasAvailable {
		log.WithFields(log.Fields{
			"service": serviceName,
			"error":   err,
		}).Warn("service became unavailable")
	}
}

func (m *ServiceHealthManager) recordFailureLocked(service *ServiceHealth, err error) {
	service.LastError = err
	service.ConsecutiveFailures++
	service.TotalFailures++
	service.LastCheck = time.Now()
	if service.ConsecutiveFailures < m.failureThreshold {
		return
	}

	alreadyOpen := service.CircuitOpen
	service.CircuitOpen = true
	service.Available = false
	service.NextRetryAt = time.Now().Add(m.recoveryTimeout)
	if !alreadyOpen {
		log.WithFields(log.Fields{
			"service":              service.Name,
			"consecutive_failures": service.ConsecutiveFailures,
			"next_retry":           service.NextRetryAt,
		}).Warn("circuit breaker opened for service")
	}
}

func cloneServiceHealth(service *ServiceHealth) *ServiceHealth {
	if service == nil {
		return nil
	}
	return &ServiceHealth{
		Name:                service.Name,
		Available:           service.Available,
		LastCheck:           service.LastCheck,
		LastSuccess:         service.LastSuccess,
		LastError:           service.LastError,
		ConsecutiveFailures: service.ConsecutiveFailures,
		CircuitOpen:         service.CircuitOpen,
		NextRetryAt:         service.NextRetryAt,
		TotalChecks:         service.TotalChecks,
		TotalFailures:       service.TotalFailures,
	}
}
