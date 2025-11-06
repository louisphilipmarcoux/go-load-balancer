package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sony/gobreaker" // NEW
	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"
)

// --- Config Structs ---
type Config struct {
	ListenAddr     string                `yaml:"listenAddr"`
	MetricsAddr    string                `yaml:"metricsAddr"`
	TLS            *TLSConfig            `yaml:"tls"`
	RateLimit      *RateLimitConfig      `yaml:"rateLimit"`
	CircuitBreaker *CircuitBreakerConfig `yaml:"circuitBreaker"` // NEW
	Routes         []*RouteConfig        `yaml:"routes"`
}

type TLSConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}

type RateLimitConfig struct {
	Enabled           bool    `yaml:"enabled"`
	RequestsPerSecond float64 `yaml:"requestsPerSecond"`
	Burst             int     `yaml:"burst"`
}

// NEW: CircuitBreakerConfig struct
type CircuitBreakerConfig struct {
	Enabled             bool          `yaml:"enabled"`
	ConsecutiveFailures uint32        `yaml:"consecutiveFailures"`
	OpenStateTimeout    time.Duration `yaml:"openStateTimeout"`
}

// ... (RouteConfig, BackendConfig, LoadConfig - no changes) ...
type RouteConfig struct {
	Path     string           `yaml:"path"`
	Strategy string           `yaml:"strategy"`
	Backends []*BackendConfig `yaml:"backends"`
}

type BackendConfig struct {
	Addr   string `yaml:"addr"`
	Weight int    `yaml:"weight"`
}

func LoadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("Warning: failed to close config file: %v", err)
		}
	}()
	var cfg Config
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config YAML: %w", err)
	}
	return &cfg, nil
}

// --- Backend Structs ---
// CHANGED: Backend now holds its own CircuitBreaker
type Backend struct {
	Addr          string
	healthy       bool
	lock          sync.RWMutex
	Weight        int
	CurrentWeight int
	connections   uint64
	cb            *gobreaker.CircuitBreaker // NEW
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

// --- BackendPool ---
type BackendPool struct {
	backends []*Backend
	strategy string
	current  uint64
	lock     sync.Mutex
}

// CHANGED: NewBackendPool now initializes circuit breakers for each backend
func NewBackendPool(strategy string, backendConfigs []*BackendConfig, cbCfg *CircuitBreakerConfig) *BackendPool {
	backends := make([]*Backend, 0, len(backendConfigs))
	for _, bc := range backendConfigs {
		weight := 1
		if bc.Weight > 0 {
			weight = bc.Weight
		}

		var cb *gobreaker.CircuitBreaker
		if cbCfg != nil && cbCfg.Enabled {
			// Create settings for this backend's circuit breaker
			st := gobreaker.Settings{
				Name: bc.Addr,
				// Count-based: trip after N consecutive failures
				ReadyToTrip: func(counts gobreaker.Counts) bool {
					return counts.ConsecutiveFailures > cbCfg.ConsecutiveFailures
				},
				// Cooldown period
				Timeout: cbCfg.OpenStateTimeout,
				// Log state changes
				OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
					log.Printf("CircuitBreaker '%s' state changed: %s -> %s", name, from, to)
				},
			}
			cb = gobreaker.NewCircuitBreaker(st)
		}

		backends = append(backends, &Backend{
			Addr:   bc.Addr,
			Weight: weight,
			cb:     cb, // NEW
		})
	}
	return &BackendPool{
		backends: backends,
		strategy: strategy,
	}
}

// ... (healthCheck - no changes) ...
func (p *BackendPool) healthCheck(b *Backend) {
	conn, err := net.DialTimeout("tcp", b.Addr, 2*time.Second)
	if err != nil {
		if b.IsHealthy() {
			log.Printf("Backend %s is DOWN", b.Addr)
			b.SetHealth(false)
		}
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("Warning: failed to close health check connection: %v", err)
		}
	}()
	if !b.IsHealthy() {
		log.Printf("Backend %s is UP", b.Addr)
		b.SetHealth(true)
	}
}

// CHANGED: StartHealthChecks now also skips health checks for "OPEN" circuits
func (p *BackendPool) StartHealthChecks() {
	log.Printf("Starting health checks for %d backends...", len(p.backends))
	for _, b := range p.backends {
		go p.healthCheck(b)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			log.Println("Running scheduled health checks...")
			for _, b := range p.backends {
				// NEW: If circuit is OPEN, don't ping it. Let it rest.
				if b.cb != nil && b.cb.State() == gobreaker.StateOpen {
					log.Printf("Skipping health check for %s (circuit OPEN)", b.Addr)
					continue
				}
				go p.healthCheck(b)
			}
		}
	}()
}

// ... (GetTotalConnections - no changes) ...
func (p *BackendPool) GetTotalConnections() uint64 {
	var total uint64
	for _, b := range p.backends {
		total += b.GetConnections()
	}
	return total
}

// --- Strategy Implementations ---

// CHANGED: All GetNext... functions now skip backends whose circuit is "OPEN"
func (p *BackendPool) GetNextBackend(r *http.Request) *Backend {
	switch p.strategy {
	case "ip-hash":
		clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			log.Printf("Failed to get client IP: %v, falling back to RoundRobin", err)
			return p.GetNextBackendByRoundRobin()
		}
		return p.GetNextBackendByIP(clientIP)
	case "least-connections":
		return p.GetNextBackendByLeastConns()
	case "weighted-least-connections":
		return p.GetNextBackendByWeightedLeastConns()
	case "weighted-round-robin":
		return p.GetNextBackendByWeightedRoundRobin()
	case "round-robin":
		fallthrough
	default:
		return p.GetNextBackendByRoundRobin()
	}
}
func (p *BackendPool) GetNextBackendByIP(ip string) *Backend {
	healthyBackends := make([]*Backend, 0)
	for _, backend := range p.backends {
		// NEW: Check health AND circuit state
		if backend.IsHealthy() && (backend.cb == nil || backend.cb.State() != gobreaker.StateOpen) {
			healthyBackends = append(healthyBackends, backend)
		}
	}
	if len(healthyBackends) == 0 {
		return nil
	}
	h := fnv.New32a()
	h.Write([]byte(ip))
	index := int(h.Sum32()) % len(healthyBackends)
	return healthyBackends[index]
}
func (p *BackendPool) GetNextBackendByLeastConns() *Backend {
	var bestBackend *Backend
	minConnections := uint64(math.MaxUint64)
	for _, backend := range p.backends {
		// NEW: Check health AND circuit state
		if !backend.IsHealthy() || (backend.cb != nil && backend.cb.State() == gobreaker.StateOpen) {
			continue
		}
		conns := backend.GetConnections()
		if conns < minConnections {
			minConnections = conns
			bestBackend = backend
		}
	}
	return bestBackend
}
func (p *BackendPool) GetNextBackendByRoundRobin() *Backend {
	numBackends := uint64(len(p.backends))
	if numBackends == 0 {
		return nil
	}
	for i := uint64(0); i < numBackends; i++ {
		index := atomic.AddUint64(&p.current, 1) % numBackends
		backend := p.backends[index]
		// NEW: Check health AND circuit state
		if backend.IsHealthy() && (backend.cb == nil || backend.cb.State() != gobreaker.StateOpen) {
			return backend
		}
	}
	return nil
}
func (p *BackendPool) GetNextBackendByWeightedRoundRobin() *Backend {
	p.lock.Lock()
	defer p.lock.Unlock()
	var bestBackend *Backend
	totalWeight := 0
	for _, backend := range p.backends {
		// NEW: Check health AND circuit state
		if !backend.IsHealthy() || (backend.cb != nil && backend.cb.State() == gobreaker.StateOpen) {
			continue
		}
		backend.CurrentWeight += backend.Weight
		totalWeight += backend.Weight
		if bestBackend == nil || backend.CurrentWeight > bestBackend.CurrentWeight {
			bestBackend = backend
		}
	}
	if bestBackend == nil {
		return nil
	}
	bestBackend.CurrentWeight -= totalWeight
	return bestBackend
}
func (p *BackendPool) GetNextBackendByWeightedLeastConns() *Backend {
	var bestBackend *Backend
	minScore := math.Inf(1)
	for _, backend := range p.backends {
		// NEW: Check health AND circuit state
		if !backend.IsHealthy() || (backend.cb != nil && backend.cb.State() == gobreaker.StateOpen) {
			continue
		}
		conns := float64(backend.GetConnections())
		weight := float64(backend.Weight)
		score := conns / weight
		if score < minScore {
			minScore = score
			bestBackend = backend
		}
	}
	return bestBackend
}

// --- Rate Limiter ---
// ... (No changes to RateLimiter) ...
type RateLimiter struct {
	clients map[string]*rate.Limiter
	lock    sync.Mutex
	rlCfg   *RateLimitConfig
}

func NewRateLimiter(rlCfg *RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		clients: make(map[string]*rate.Limiter),
		rlCfg:   rlCfg,
	}
}

func (rl *RateLimiter) GetLimiter(ip string) *rate.Limiter {
	rl.lock.Lock()
	defer rl.lock.Unlock()

	limiter, exists := rl.clients[ip]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(rl.rlCfg.RequestsPerSecond), rl.rlCfg.Burst)
		rl.clients[ip] = limiter
	}
	return limiter
}

// --- Proxy and LB ---
// ... (LoadBalancer struct - no changes) ...
type LoadBalancer struct {
	cfg   *Config
	mux   *http.ServeMux
	pools map[string]*BackendPool
	rl    *RateLimiter
	lock  sync.RWMutex
}

// CHANGED: buildState now passes circuit breaker config to NewBackendPool
func (lb *LoadBalancer) buildState(cfg *Config) (*http.ServeMux, map[string]*BackendPool) {
	mux := http.NewServeMux()
	pools := make(map[string]*BackendPool)

	for _, route := range cfg.Routes {
		route := route
		// NEW: Pass circuit breaker config
		pool := NewBackendPool(route.Strategy, route.Backends, cfg.CircuitBreaker)
		pool.StartHealthChecks()
		pools[route.Path] = pool

		// Create the HTTP handler for this route
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			backend := pool.GetNextBackend(r)
			if backend == nil {
				log.Printf("No healthy, non-tripped backend found for path: %s", route.Path)
				http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
				return
			}

			// ... (Connection counting - no changes) ...
			strategyNeedsConns := pool.strategy == "least-connections" ||
				pool.strategy == "weighted-least-connections"
			if strategyNeedsConns {
				backend.IncrementConnections()
				defer func() {
					backend.DecrementConnections()
				}()
			}

			targetUrl, err := url.Parse("http://" + backend.Addr)
			if err != nil {
				log.Printf("Failed to parse backend URL: %v", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			proxy := httputil.NewSingleHostReverseProxy(targetUrl)

			// --- CIRCUIT BREAKER LOGIC ---
			if backend.cb != nil {
				// Execute the request *through* the circuit breaker
				_, err := backend.cb.Execute(func() (interface{}, error) {
					// This anonymous function is the "protected" action
					log.Printf("L7 Proxy: %s %s -> %s (CB: %s)", r.Method, r.URL.Path, backend.Addr, backend.cb.State())

					// We need a custom ErrorHandler to detect 5xx errors
					proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
						// This error is a *connection* failure (e.g., dial error)
						log.Printf("Backend error: %v", err)
						// We'll return this error to the circuit breaker
						// We must not write a response here, or the circuit breaker
						// wrapper will panic.
					}

					// We need a custom ResponseModifier to detect 5xx responses
					// This is a *bit* of a hack, as we're just checking status
					// We use a custom response writer to capture the status code
					recorder := &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}

					proxy.ServeHTTP(recorder, r)

					// After the request, check the status code
					if recorder.Status >= http.StatusInternalServerError {
						// It was a 5xx error. Return an error to trip the circuit.
						return nil, fmt.Errorf("backend %s returned status %d", backend.Addr, recorder.Status)
					}

					// No error
					return nil, nil
				})

				// After Execute, check if the circuit was open
				if err != nil {
					// If err is gobreaker.ErrOpenState, the request was blocked
					// If err is gobreaker.ErrTooManyRequests, it was blocked (HALF-OPEN)
					// If it's any other error, it means our *protected function* failed

					if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
						log.Printf("Request blocked for %s (CB: %s)", backend.Addr, backend.cb.State())
						http.Error(w, "Service unavailable (Circuit OPEN)", http.StatusServiceUnavailable)
					} else {
						// This was the 5xx error or connection error
						// The proxy.ErrorHandler already logged it, but the
						// ResponseWriter (recorder) already wrote the bad response.
						// We just need to log that the failure was recorded.
						log.Printf("Failure recorded for %s: %v", backend.Addr, err)
					}
				}
				// If err is nil, the request succeeded, and response was written
			} else {
				// No circuit breaker, just proxy directly
				log.Printf("L7 Proxy: %s %s -> %s", r.Method, r.URL.Path, backend.Addr)
				proxy.ServeHTTP(w, r)
			}
			// --- END CIRCUIT BREAKER LOGIC ---
		})
		mux.Handle(route.Path, handler)
	}
	return mux, pools
}

// NEW: StatusRecorder to capture the HTTP status code for the circuit breaker
type StatusRecorder struct {
	http.ResponseWriter
	Status int
}

func (r *StatusRecorder) WriteHeader(status int) {
	r.Status = status
	r.ResponseWriter.WriteHeader(status)
}

// ... (NewLoadBalancer, ServeHTTP, ReloadConfig, StartMetricsServer - no changes) ...
func NewLoadBalancer(cfg *Config) *LoadBalancer {
	lb := &LoadBalancer{cfg: cfg}
	lb.mux, lb.pools = lb.buildState(cfg)
	if cfg.RateLimit != nil && cfg.RateLimit.Enabled {
		lb.rl = NewRateLimiter(cfg.RateLimit)
	}
	return lb
}
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// --- RATE LIMITING LOGIC ---
	lb.lock.RLock()
	limiterEnabled := lb.rl != nil
	lb.lock.RUnlock()

	if limiterEnabled {
		clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			log.Printf("Failed to get client IP for rate limiting: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		ipLimiter := lb.rl.GetLimiter(clientIP)
		if !ipLimiter.Allow() {
			log.Printf("Rate limit exceeded for IP: %s", clientIP)
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}
	}
	// --- END RATE LIMITING LOGIC ---

	lb.lock.RLock()
	mux := lb.mux
	lb.lock.RUnlock()
	mux.ServeHTTP(w, r)
}
func (lb *LoadBalancer) ReloadConfig(path string) error {
	log.Println("Reloading configuration from", path)
	cfg, err := LoadConfig(path)
	if err != nil {
		return fmt.Errorf("failed to load new config: %w", err)
	}

	newMux, newPools := lb.buildState(cfg)
	var newRl *RateLimiter
	if cfg.RateLimit != nil && cfg.RateLimit.Enabled {
		newRl = NewRateLimiter(cfg.RateLimit)
	}

	lb.lock.Lock()
	lb.cfg = cfg
	lb.mux = newMux
	lb.pools = newPools
	lb.rl = newRl
	lb.lock.Unlock()

	log.Println("Configuration successfully reloaded.")
	return nil
}
func StartMetricsServer(addr string, lb *LoadBalancer) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		lb.lock.RLock()
		pools := lb.pools
		lb.lock.RUnlock()

		var body string
		var totalConns uint64

		for path, pool := range pools {
			totalConns += pool.GetTotalConnections()
			for _, b := range pool.backends {
				health := 0
				if b.IsHealthy() {
					health = 1
				}
				body += fmt.Sprintf("backend_health_status{backend=\"%s\", route=\"%s\"} %d\n", b.Addr, path, health)
				conns := b.GetConnections()
				body += fmt.Sprintf("backend_active_connections{backend=\"%s\", route=\"%s\"} %d\n", b.Addr, path, conns)

				// NEW: Export circuit breaker state
				if b.cb != nil {
					state := b.cb.State()
					var stateNum int
					switch state {
					case gobreaker.StateClosed:
						stateNum = 0
					case gobreaker.StateHalfOpen:
						stateNum = 1
					case gobreaker.StateOpen:
						stateNum = 2
					}
					body += fmt.Sprintf("backend_circuit_state{backend=\"%s\", route=\"%s\"} %d\n", b.Addr, path, stateNum)
				}
			}
		}
		body += fmt.Sprintf("total_active_connections %d\n", totalConns)

		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte(body)); err != nil {
			log.Printf("Warning: failed to write metrics response: %v", err)
		}
	})

	log.Printf("Metrics server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("Metrics server failed: %v", err)
	}
}

// ... (main - no changes) ...
func main() {
	configPath := "config.yaml"
	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	lb := NewLoadBalancer(cfg)

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: lb,
	}

	if cfg.MetricsAddr != "" {
		go StartMetricsServer(cfg.MetricsAddr, lb)
	}

	go func() {
		if cfg.TLS != nil {
			log.Printf("Load Balancer (L7/TLS) listening on %s", server.Addr)
			if err := server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Failed to run TLS load balancer: %v", err)
			}
		} else {
			log.Printf("Load Balancer (L7/HTTP) listening on %s", server.Addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Failed to run HTTP load balancer: %v", err)
			}
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for sig := range sigChan {
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			log.Println("Shutting down... Stopping new connections.")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := server.Shutdown(ctx); err != nil {
				log.Printf("Graceful shutdown failed: %v", err)
			} else {
				log.Println("Graceful shutdown complete.")
			}
			return

		case syscall.SIGHUP:
			if err := lb.ReloadConfig(configPath); err != nil {
				log.Printf("Failed to reload config: %v", err)
			}
		}
	}
}
