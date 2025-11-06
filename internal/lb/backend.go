package lb

import (
	"log/slog"
	"net"
	"net/http"          // NEW
	"net/http/httputil" // NEW
	"net/url"
	"strings" // NEW
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

// CHANGED: NewBackendPool now creates the shared transport and proxies
func NewBackendPool(
	strategy string,
	backendConfigs []*BackendConfig,
	cbCfg *CircuitBreakerConfig,
	poolCfg *ConnectionPoolConfig,
) *BackendPool {

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
		weight := 1
		if bc.Weight > 0 {
			weight = bc.Weight
		}
		var cb *gobreaker.CircuitBreaker
		if cbCfg != nil && cbCfg.Enabled {
			st := gobreaker.Settings{
				Name: bc.Addr,
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

		parsedURL, err := url.Parse("http://" + bc.Addr)
		if err != nil {
			slog.Error("Failed to parse backend URL", "url", bc.Addr, "error", err)
			continue
		}

		proxy := httputil.NewSingleHostReverseProxy(parsedURL)
		proxy.Transport = transport

		// Save the default director created by NewSingleHostReverseProxy
		originalDirector := proxy.Director

		// Create a new director that wraps the original
		proxy.Director = func(r *http.Request) {
			// Get the original request's host and protocol
			originalHost := r.Host
			originalProto := "http"
			if r.TLS != nil {
				originalProto = "https"
			}

			// Run the default director first
			originalDirector(r)

			// --- Now, add/modify our headers ---
			// Get the real client IP
			clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

			// Add/Append the X-Forwarded-For header
			if prior, ok := r.Header["X-Forwarded-For"]; ok {
				// If it already exists, append our client IP
				r.Header.Set("X-Forwarded-For", strings.Join(prior, ", ")+", "+clientIP)
			} else {
				// Otherwise, create it
				r.Header.Set("X-Forwarded-For", clientIP)
			}

			// Set the X-Forwarded-Host to the original host
			r.Header.Set("X-Forwarded-Host", originalHost)
			// Set the X-Forwarded-Proto
			r.Header.Set("X-Forwarded-Proto", originalProto)
		}

		backends = append(backends, &Backend{
			Addr:      bc.Addr,
			Weight:    weight,
			cb:        cb,
			parsedURL: parsedURL,
			proxy:     proxy,
		})
	}
	return &BackendPool{
		backends: backends,
		strategy: strategy,
	}
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
