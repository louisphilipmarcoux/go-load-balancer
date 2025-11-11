package lb

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker"
)

// Backend struct and its methods
type Backend struct {
	Addr          string
	healthy       bool
	lock          sync.RWMutex
	Weight        int
	CurrentWeight int
	connections   uint64
	cb            *gobreaker.CircuitBreaker
	parsedURL     *url.URL               // L7 only
	proxy         *httputil.ReverseProxy // L7 only
}

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

// BackendPool struct
type BackendPool struct {
	backends []*Backend
	strategy string
	current  uint64
	lock     sync.Mutex
}

// --- L7 Backend Functions ---

// newL7Backend creates an L7 (HTTP) backend
func (p *BackendPool) newL7Backend(
	beConfig *BackendConfig,
	cbCfg *CircuitBreakerConfig,
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

// NewL7BackendPool creates a pool of L7 backends
func NewL7BackendPool(
	routeCfg *RouteConfig,
	discoverer ServiceDiscoverer,
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig,
) *BackendPool {

	pool := &BackendPool{
		strategy: routeCfg.Strategy,
		backends: make([]*Backend, 0),
	}

	if routeCfg.Service != "" {
		if discoverer == nil {
			slog.Error("Service discovery configured but discoverer is nil", "service", routeCfg.Service)
			return pool
		}
		poolCfgForWatch := poolCfg
		if poolCfgForWatch == nil {
			poolCfgForWatch = &ConnectionPoolConfig{}
		}
		discoverer.WatchService(routeCfg.Service, pool, cbCfg, poolCfgForWatch)
	} else {
		staticBackends := make(map[string]*BackendConfig)
		for _, bc := range routeCfg.Backends {
			staticBackends[bc.Addr] = bc
		}
		pool.buildL7Backends(staticBackends, cbCfg, poolCfg)
		pool.StartHealthChecks()
	}
	return pool
}

// buildL7Backends populates the pool with L7 backends
func (p *BackendPool) buildL7Backends(
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
		if be := p.newL7Backend(bc, cbCfg, transport); be != nil {
			backends = append(backends, be)
		}
	}
	p.backends = backends
}

// UpdateL7Backends atomically updates the pool with L7 backends
func (p *BackendPool) UpdateL7Backends(
	newBackends map[string]*BackendConfig,
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig,
) {
	p.lock.Lock()
	defer p.lock.Unlock()

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
			existing.Weight = beConfig.Weight
			if existing.Weight == 0 {
				existing.Weight = 1
			}
			existing.SetHealth(true)
			finalBackends = append(finalBackends, existing)
		} else {
			slog.Info("Service discovery: new L7 backend found", "addr", addr)
			if be := p.newL7Backend(beConfig, cbCfg, transport); be != nil {
				be.SetHealth(true)
				finalBackends = append(finalBackends, be)
			}
		}
	}

	for addr, be := range currentBackends {
		if _, ok := newBackends[addr]; !ok {
			slog.Warn("Service discovery: L7 backend removed", "addr", addr)
			be.SetHealth(false)
			finalBackends = append(finalBackends, be)
		}
	}

	p.backends = finalBackends
	slog.Info("L7 Backend pool updated via service discovery", "service", p.strategy, "count", len(p.backends))
}

// --- L4 Backend Functions ---

// newL4Backend creates an L4 (TCP/UDP) backend
func (p *BackendPool) newL4Backend(
	beConfig *BackendConfig,
	cbCfg *CircuitBreakerConfig,
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

	return &Backend{
		Addr:   beConfig.Addr,
		Weight: weight,
		cb:     cb,
	}
}

// NewL4BackendPool creates a pool of L4 backends
func NewL4BackendPool(
	listenerCfg *ListenerConfig,
	discoverer ServiceDiscoverer,
	cbCfg *CircuitBreakerConfig,
) *BackendPool {

	pool := &BackendPool{
		strategy: listenerCfg.Strategy,
		backends: make([]*Backend, 0),
	}

	// --- THIS IS THE FIX ---
	// We check the protocol before deciding on health check logic
	if listenerCfg.Service != "" {
		if discoverer == nil {
			slog.Error("Service discovery configured but discoverer is nil", "service", listenerCfg.Service)
			return pool
		}
		discoverer.WatchService(listenerCfg.Service, pool, cbCfg, nil)
		// We still have a problem: discovered UDP backends will be marked
		// healthy, but static ones won't. This logic needs refinement,
		// but let's fix the static case first.
	} else {
		staticBackends := make(map[string]*BackendConfig)
		for _, bc := range listenerCfg.Backends {
			staticBackends[bc.Addr] = bc
		}
		pool.buildL4Backends(staticBackends, cbCfg)

		// --- THIS IS THE FIX ---
		switch listenerCfg.Protocol {
		case "tcp":
			pool.StartHealthChecks()
		case "udp":
			// For UDP, we can't do a TCP dial.
			// Assume healthy for now.
			slog.Warn("Skipping health checks for UDP listener, marking all backends as healthy", "listener", listenerCfg.Name)
			for _, b := range pool.backends {
				b.SetHealth(true)
			}
		}
	}
	return pool
}

// buildL4Backends populates the pool with L4 backends
func (p *BackendPool) buildL4Backends(
	backendConfigs map[string]*BackendConfig,
	cbCfg *CircuitBreakerConfig,
) {
	backends := make([]*Backend, 0, len(backendConfigs))
	for _, bc := range backendConfigs {
		if be := p.newL4Backend(bc, cbCfg); be != nil {
			backends = append(backends, be)
		}
	}
	p.backends = backends
}

// UpdateL4Backends atomically updates the pool with L4 backends
func (p *BackendPool) UpdateL4Backends(
	newBackends map[string]*BackendConfig,
	cbCfg *CircuitBreakerConfig,
) {
	p.lock.Lock()
	defer p.lock.Unlock()

	currentBackends := make(map[string]*Backend)
	finalBackends := make([]*Backend, 0, len(newBackends))

	for _, be := range p.backends {
		currentBackends[be.Addr] = be
	}

	for addr, beConfig := range newBackends {
		if existing, ok := currentBackends[addr]; ok {
			existing.Weight = beConfig.Weight
			if existing.Weight == 0 {
				existing.Weight = 1
			}
			existing.SetHealth(true)
			finalBackends = append(finalBackends, existing)
		} else {
			slog.Info("Service discovery: new L4 backend found", "addr", addr)
			if be := p.newL4Backend(beConfig, cbCfg); be != nil {
				be.SetHealth(true)
				finalBackends = append(finalBackends, be)
			}
		}
	}

	for addr, be := range currentBackends {
		if _, ok := newBackends[addr]; !ok {
			slog.Warn("Service discovery: L4 backend removed", "addr", addr)
			be.SetHealth(false)
			finalBackends = append(finalBackends, be)
		}
	}

	p.backends = finalBackends
	slog.Info("L4 Backend pool updated via service discovery", "service", p.strategy, "count", len(p.backends))
}

// --- Common Functions ---

// healthCheck performs a simple TCP dial
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

// StartHealthChecks starts health checks for static backends
func (p *BackendPool) StartHealthChecks() {
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

// GetTotalConnections returns the sum of active connections for all backends
func (p *BackendPool) GetTotalConnections() uint64 {
	var total uint64
	for _, b := range p.backends {
		total += b.GetConnections()
	}
	return total
}
