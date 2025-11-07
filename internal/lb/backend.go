package lb

import (
	"log/slog"
	"net"
	"net/http"          // NEW
	"net/http/httputil" // NEW
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker"
)

// Backend holds the state for a single backend server
type Backend struct {
	Addr          string
	healthy       bool
	lock          sync.RWMutex
	Weight        int
	CurrentWeight int
	connections   uint64
	cb            *gobreaker.CircuitBreaker
	parsedURL     *url.URL               // NEW: Store the parsed URL
	proxy         *httputil.ReverseProxy // NEW: Store the proxy
}

// ... (IsHealthy, SetHealth, Inc/Dec/GetConnections - no changes) ...
func (b *Backend) IsHealthy() bool {
	b.lock.RLock()
	defer b.lock.RUnlock()
	return b.healthy
}
func (b *Backend) SetHealth(healthy bool) {
	b.lock.Lock()
	defer b.lock.Unlock()
	b.healthy = healthy
}
func (b *Backend) IncrementConnections() {
	atomic.AddUint64(&b.connections, 1)
}
func (b *Backend) DecrementConnections() {
	atomic.AddUint64(&b.connections, ^uint64(0))
}
func (b *Backend) GetConnections() uint64 {
	return atomic.LoadUint64(&b.connections)
}

// ... (BackendPool struct - no changes) ...
type BackendPool struct {
	backends []*Backend
	strategy string
	current  uint64
	lock     sync.Mutex
}

// newBackend constructs a single Backend.
// We pass cbCfg and poolCfg so we can re-use them.
func (p *BackendPool) newBackend(
	beConfig *BackendConfig,
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig,
	transport *http.Transport,
) *Backend {

	weight := 1
	if beConfig.Weight > 0 {
		weight = beConfig.Weight
	}

	var cb *gobreaker.CircuitBreaker
	if cbCfg != nil && cbCfg.Enabled {
		st := gobreaker.Settings{
			Name: beConfig.Addr,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures > cbCfg.ConsecutiveFailures
			},
			Timeout: cbCfg.OpenStateTimeout,
			OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
				slog.Warn("CircuitBreaker state changed", "backend", name, "from", from.String(), "to", to.String())
			},
		}
		cb = gobreaker.NewCircuitBreaker(st)
	}

	parsedURL, err := url.Parse("http://" + beConfig.Addr)
	if err != nil {
		slog.Error("Failed to parse backend URL", "url", beConfig.Addr, "error", err)
		return nil
	}

	proxy := httputil.NewSingleHostReverseProxy(parsedURL)
	proxy.Transport = transport
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalHost := r.Host
		originalProto := "http"
		if r.TLS != nil {
			originalProto = "https"
		}
		originalDirector(r)
		r.Header.Set("X-Forwarded-Host", originalHost)
		r.Header.Set("X-Forwarded-Proto", originalProto)
	}

	return &Backend{
		Addr:      beConfig.Addr,
		Weight:    weight,
		cb:        cb,
		parsedURL: parsedURL,
		proxy:     proxy,
	}
}

// CHANGED: NewBackendPool now creates the shared transport and proxies
func NewBackendPool(
	routeCfg *RouteConfig, // <-- CHANGED: Pass the whole RouteConfig
	consul *ConsulClient, // <-- ADD THIS
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig,
) *BackendPool {

	pool := &BackendPool{
		strategy: routeCfg.Strategy,
		backends: make([]*Backend, 0),
	}

	// If a service name is provided, use service discovery
	if routeCfg.Service != "" {
		if consul == nil {
			slog.Error("Service discovery configured but Consul client is nil", "service", routeCfg.Service)
			return pool // Return an empty, non-functional pool
		}
		// Start watching Consul for updates.
		// UpdateBackends will be called with the initial list and all changes.
		consul.WatchService(routeCfg.Service, pool, cbCfg, poolCfg)

	} else {
		// No service discovery, use static backends from config
		// This uses a map for the initial build
		staticBackends := make(map[string]*BackendConfig)
		for _, bc := range routeCfg.Backends {
			staticBackends[bc.Addr] = bc
		}
		// Build the initial list
		pool.buildBackends(staticBackends, cbCfg, poolCfg)
		// Start our own health checks
		pool.StartHealthChecks()
	}

	return pool
}

func (p *BackendPool) buildBackends(
	backendConfigs map[string]*BackendConfig,
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig,
) {

	var transport *http.Transport
	if poolCfg != nil {
		transport = &http.Transport{
			MaxIdleConns:        poolCfg.MaxIdleConns,
			MaxIdleConnsPerHost: poolCfg.MaxIdleConnsPerHost,
			IdleConnTimeout:     poolCfg.IdleConnTimeout,
		}
	} else {
		transport = &http.Transport{}
	}

	backends := make([]*Backend, 0, len(backendConfigs))
	for _, bc := range backendConfigs {
		if be := p.newBackend(bc, cbCfg, poolCfg, transport); be != nil {
			backends = append(backends, be)
		}
	}
	p.backends = backends
}

// This method atomically updates the list of backends in the pool
func (p *BackendPool) UpdateBackends(
	newBackends map[string]*BackendConfig,
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig,
) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// We need to re-create the transport
	var transport *http.Transport
	if poolCfg != nil {
		transport = &http.Transport{
			MaxIdleConns:        poolCfg.MaxIdleConns,
			MaxIdleConnsPerHost: poolCfg.MaxIdleConnsPerHost,
			IdleConnTimeout:     poolCfg.IdleConnTimeout,
		}
	} else {
		transport = &http.Transport{}
	}

	currentBackends := make(map[string]*Backend)
	finalBackends := make([]*Backend, 0, len(newBackends))

	for _, be := range p.backends {
		currentBackends[be.Addr] = be
	}

	for addr, beConfig := range newBackends {
		if existing, ok := currentBackends[addr]; ok {
			// Backend already exists, just update weight and add it
			existing.Weight = beConfig.Weight
			if existing.Weight == 0 {
				existing.Weight = 1
			}
			// IMPORTANT: We must also update its health status
			existing.SetHealth(true) // Consul says it's passing
			finalBackends = append(finalBackends, existing)

		} else {
			// New backend, create it with all features
			slog.Info("Service discovery: new backend found", "addr", addr)
			if be := p.newBackend(beConfig, cbCfg, poolCfg, transport); be != nil {
				be.SetHealth(true) // Consul says it's passing
				finalBackends = append(finalBackends, be)
			}
		}
	}

	// Mark any backends that are in our list but not in Consul's
	for addr, be := range currentBackends {
		if _, ok := newBackends[addr]; !ok {
			slog.Warn("Service discovery: backend removed", "addr", addr)
			be.SetHealth(false)
			finalBackends = append(finalBackends, be) // Keep it, but mark as down
		}
	}

	p.backends = finalBackends
	slog.Info("Backend pool updated via service discovery", "service", p.strategy, "count", len(p.backends))
}

func (p *BackendPool) healthCheck(b *Backend) {
	conn, err := net.DialTimeout("tcp", b.Addr, 2*time.Second)
	if err != nil {
		if b.IsHealthy() {
			slog.Warn("Backend health check failed", "backend", b.Addr, "status", "DOWN", "error", err)
			b.SetHealth(false)
		}
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Warn("Failed to close health check connection", "backend", b.Addr, "error", err)
		}
	}()
	if !b.IsHealthy() {
		slog.Info("Backend health check successful", "backend", b.Addr, "status", "UP")
		b.SetHealth(true)
	}
}

func (p *BackendPool) StartHealthChecks() {
	// Only run if we have backends (i.e., not in service discovery mode)
	if len(p.backends) == 0 {
		slog.Debug("Skipping health checks for service discovery pool")
		return
	}

	slog.Info("Starting health checks", "backend_count", len(p.backends))
	for _, b := range p.backends {
		go p.healthCheck(b)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			slog.Debug("Running scheduled health checks...")
			for _, b := range p.backends {
				if b.cb != nil && b.cb.State() == gobreaker.StateOpen {
					slog.Debug("Skipping health check for open circuit", "backend", b.Addr)
					continue
				}
				go p.healthCheck(b)
			}
		}
	}()
}
func (p *BackendPool) GetTotalConnections() uint64 {
	var total uint64
	for _, b := range p.backends {
		total += b.GetConnections()
	}
	return total
}
