package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	// "io" // No longer needed
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

	"gopkg.in/yaml.v3"
)

// --- Config Structs ---
// CHANGED: Added TLS
type Config struct {
	ListenAddr  string         `yaml:"listenAddr"`
	MetricsAddr string         `yaml:"metricsAddr"`
	TLS         *TLSConfig     `yaml:"tls"` // NEW
	Routes      []*RouteConfig `yaml:"routes"`
}

// NEW: TLSConfig struct
type TLSConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
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
// ... (No changes to Backend struct or its methods) ...
type Backend struct {
	Addr          string
	healthy       bool
	lock          sync.RWMutex
	Weight        int
	CurrentWeight int
	connections   uint64
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

// --- BackendPool ---
// ... (NewBackendPool, healthCheck, StartHealthChecks, GetTotalConnections - no changes) ...
type BackendPool struct {
	backends []*Backend
	strategy string
	current  uint64
	lock     sync.Mutex
}

func NewBackendPool(strategy string, backendConfigs []*BackendConfig) *BackendPool {
	backends := make([]*Backend, 0, len(backendConfigs))
	for _, bc := range backendConfigs {
		weight := 1
		if bc.Weight > 0 {
			weight = bc.Weight
		}
		backends = append(backends, &Backend{
			Addr:   bc.Addr,
			Weight: weight,
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

// --- Strategy Implementations ---
// ... (GetNextBackend, GetNextBackendByIP, ...etc - no changes) ...
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
		if backend.IsHealthy() {
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
		if !backend.IsHealthy() {
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
		if backend.IsHealthy() {
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
		if !backend.IsHealthy() {
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
		if !backend.IsHealthy() {
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

// --- Proxy and LB ---
// ... (LoadBalancer struct, buildState, NewLoadBalancer, ServeHTTP, ReloadConfig - no changes) ...
type LoadBalancer struct {
	cfg   *Config
	mux   *http.ServeMux
	pools map[string]*BackendPool // map[path] -> *BackendPool
	lock  sync.RWMutex
}

func (lb *LoadBalancer) buildState(cfg *Config) (*http.ServeMux, map[string]*BackendPool) {
	mux := http.NewServeMux()
	pools := make(map[string]*BackendPool)

	for _, route := range cfg.Routes {
		// Capture loop variables for the closure
		route := route
		pool := NewBackendPool(route.Strategy, route.Backends)
		pool.StartHealthChecks()
		pools[route.Path] = pool

		// Create the HTTP handler for this route
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			backend := pool.GetNextBackend(r)
			if backend == nil {
				log.Printf("No healthy backend found for path: %s", route.Path)
				http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
				return
			}

			// Increment/Decrement connections if needed
			strategyNeedsConns := pool.strategy == "least-connections" ||
				pool.strategy == "weighted-least-connections"
			if strategyNeedsConns {
				backend.IncrementConnections()
				// Use defer in a closure to ensure it runs *after* proxy.ServeHTTP
				defer func() {
					backend.DecrementConnections()
				}()
			}

			// Create the reverse proxy
			targetUrl, err := url.Parse("http://" + backend.Addr)
			if err != nil {
				log.Printf("Failed to parse backend URL: %v", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			proxy := httputil.NewSingleHostReverseProxy(targetUrl)

			// Log the L7 proxy action
			log.Printf("L7 Proxy: %s %s -> %s", r.Method, r.URL.Path, backend.Addr)

			// proxy.ServeHTTP handles the request
			proxy.ServeHTTP(w, r)
		})
		mux.Handle(route.Path, handler)
	}
	return mux, pools
}

func NewLoadBalancer(cfg *Config) *LoadBalancer {
	lb := &LoadBalancer{cfg: cfg}
	lb.mux, lb.pools = lb.buildState(cfg)
	return lb
}

func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	lb.lock.Lock()
	lb.cfg = cfg
	lb.mux = newMux
	lb.pools = newPools
	lb.lock.Unlock()

	log.Println("Configuration successfully reloaded.")
	return nil
}

// ... (StartMetricsServer - no changes) ...
func StartMetricsServer(addr string, lb *LoadBalancer) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		lb.lock.RLock()
		pools := lb.pools // Get the current map of pools
		lb.lock.RUnlock()

		var body string
		var totalConns uint64

		// Loop over each route and its pool
		for path, pool := range pools {
			totalConns += pool.GetTotalConnections()
			for _, b := range pool.backends {
				health := 0
				if b.IsHealthy() {
					health = 1
				}
				// Add a "route" label to distinguish backends
				body += fmt.Sprintf("backend_health_status{backend=\"%s\", route=\"%s\"} %d\n", b.Addr, path, health)
				conns := b.GetConnections()
				body += fmt.Sprintf("backend_active_connections{backend=\"%s\", route=\"%s\"} %d\n", b.Addr, path, conns)
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

// CHANGED: main now checks for TLS config
func main() {
	configPath := "config.yaml"
	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Create the central LoadBalancer (which is an http.Handler)
	lb := NewLoadBalancer(cfg)

	// Create the main HTTP server
	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: lb,
	}

	// Start the metrics server in a goroutine
	if cfg.MetricsAddr != "" {
		go StartMetricsServer(cfg.MetricsAddr, lb)
	}

	// Start the main load balancer server in a goroutine
	go func() {
		// --- THIS IS THE KEY CHANGE ---
		if cfg.TLS != nil {
			// Start an HTTPS server
			log.Printf("Load Balancer (L7/TLS) listening on %s", server.Addr)
			if err := server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Failed to run TLS load balancer: %v", err)
			}
		} else {
			// Start a plain HTTP server
			log.Printf("Load Balancer (L7/HTTP) listening on %s", server.Addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Failed to run HTTP load balancer: %v", err)
			}
		}
	}()

	// --- Signal handling for shutdown AND reload ---
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for sig := range sigChan {
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			// --- Graceful Shutdown ---
			log.Println("Shutting down... Stopping new connections.")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := server.Shutdown(ctx); err != nil {
				log.Printf("Graceful shutdown failed: %v", err)
			} else {
				log.Println("Graceful shutdown complete.")
			}
			return // Exit main

		case syscall.SIGHUP:
			// --- Hot Reload ---
			if err := lb.ReloadConfig(configPath); err != nil {
				log.Printf("Failed to reload config: %v", err)
			}
		}
	}
}
