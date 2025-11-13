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

// newL7Backend (unchanged)
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

// NewL7BackendPool (CHANGED)
func NewL7BackendPool(
	routeCfg *RouteConfig,
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig,
) *BackendPool {

	pool := &BackendPool{
		strategy: routeCfg.Strategy,
		backends: make([]*Backend, 0),
	}

	// Service discovery logic is removed
	staticBackends := make(map[string]*BackendConfig)
	for _, bc := range routeCfg.Backends {
		staticBackends[bc.Addr] = bc
	}
	pool.buildL7Backends(staticBackends, cbCfg, poolCfg)
	pool.StartHealthChecks()

	return pool
}

// buildL7Backends (unchanged)
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

// UpdateL7Backends is DELETED

// --- L4 Backend Functions ---

// newL4Backend (unchanged)
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

// NewL4BackendPool (CHANGED)
func NewL4BackendPool(
	listenerCfg *ListenerConfig,
	cbCfg *CircuitBreakerConfig,
) *BackendPool {

	pool := &BackendPool{
		strategy: listenerCfg.Strategy,
		backends: make([]*Backend, 0),
	}

	// Service discovery logic is removed
	staticBackends := make(map[string]*BackendConfig)
	for _, bc := range listenerCfg.Backends {
		staticBackends[bc.Addr] = bc
	}
	pool.buildL4Backends(staticBackends, cbCfg)

	switch listenerCfg.Protocol {
	case "tcp":
		pool.StartHealthChecks()
	case "udp":
		slog.Warn("Skipping health checks for UDP listener, marking all backends as healthy", "listener", listenerCfg.Name)
		for _, b := range pool.backends {
			b.SetHealth(true)
		}
	}

	return pool
}

// buildL4Backends (unchanged)
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

// UpdateL4Backends is DELETED

// --- Common Functions ---

// healthCheck (unchanged)
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

// StartHealthChecks (CHANGED - simplified log)
func (p *BackendPool) StartHealthChecks() {
	if len(p.backends) == 0 {
		slog.Debug("Skipping health checks for empty pool")
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

// GetTotalConnections (unchanged)
func (p *BackendPool) GetTotalConnections() uint64 {
	var total uint64
	for _, b := range p.backends {
		total += b.GetConnections()
	}
	return total
}
