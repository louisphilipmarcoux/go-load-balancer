package lb

// ServiceDiscoverer defines the interface for watching a service.
// This decouples the BackendPool from any specific implementation like Consul.
type ServiceDiscoverer interface {
	// WatchService monitors a service for changes and updates the
	// provided BackendPool with the new list of backends.
	WatchService(
		serviceName string,
		pool *BackendPool,
		cbCfg *CircuitBreakerConfig,
		poolCfg *ConnectionPoolConfig, // nil for L4
	)
}
