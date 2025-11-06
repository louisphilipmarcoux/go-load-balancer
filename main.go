package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math"
	"net"
	"net/http" // NEW: Import net/http
	"os"
	"os/signal" // NEW: Import strconv
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// --- Config Structs ---
type Config struct {
	ListenAddr  string           `yaml:"listenAddr"`
	Strategy    string           `yaml:"strategy"`
	MetricsAddr string           `yaml:"metricsAddr"` // NEW
	Backends    []*BackendConfig `yaml:"backends"`
}

// ... (BackendConfig, LoadConfig - no changes) ...
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
// ... (Backend struct and methods - no changes) ...
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
// ... (BackendPool struct, NewBackendPool, healthCheck, StartHealthChecks - no changes) ...
type BackendPool struct {
	backends []*Backend
	strategy string
	current  uint64
	lock     sync.Mutex
}

func NewBackendPool(cfg *Config) *BackendPool {
	backends := make([]*Backend, 0, len(cfg.Backends))
	for _, bc := range cfg.Backends {
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
		strategy: cfg.Strategy,
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
	log.Println("Starting health checks...")
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

// NEW: GetTotalConnections calculates the sum of all backend connections
func (p *BackendPool) GetTotalConnections() uint64 {
	var total uint64
	for _, b := range p.backends {
		total += b.GetConnections()
	}
	return total
}

// --- Strategy Implementations ---
// ... (All strategy functions - no changes) ...
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
// ... (handleProxy - no changes) ...
func handleProxy(client, backend net.Conn) {
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("Warning: failed to close client connection: %v", err)
		}
	}()
	defer func() {
		if err := backend.Close(); err != nil {
			log.Printf("Warning: failed to close backend connection: %v", err)
		}
	}()
	log.Printf("Proxying traffic for %s to %s", client.RemoteAddr(), backend.RemoteAddr())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(backend, client); err != nil {
			log.Printf("Error copying from client to backend: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(client, backend); err != nil {
			log.Printf("Error copying from backend to client: %v", err)
		}
	}()
	wg.Wait()
	log.Printf("Connection for %s closed", client.RemoteAddr())
}

// ... (RunLoadBalancer - no changes) ...
func RunLoadBalancer(cfg *Config, pool *BackendPool, listener net.Listener, wg *sync.WaitGroup) error {
	log.Printf("Load Balancer listening on %s, strategy: %s", listener.Addr().String(), cfg.Strategy)
	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				log.Println("Listener closed. No longer accepting new connections.")
				return nil
			}
			log.Printf("Failed to accept connection: %v", err)
			continue
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			var backend *Backend
			strategyNeedsConns := pool.strategy == "least-connections" ||
				pool.strategy == "weighted-least-connections"
			switch pool.strategy {
			case "ip-hash":
				clientIP, _, err := net.SplitHostPort(c.RemoteAddr().String())
				if err != nil {
					log.Printf("Failed to get client IP: %v", err)
					if err := c.Close(); err != nil {
						log.Printf("Warning: failed to close client connection on IP error: %v", err)
					}
					return
				}
				backend = pool.GetNextBackendByIP(clientIP)
			case "least-connections":
				backend = pool.GetNextBackendByLeastConns()
			case "weighted-least-connections":
				backend = pool.GetNextBackendByWeightedLeastConns()
			case "weighted-round-robin":
				backend = pool.GetNextBackendByWeightedRoundRobin()
			case "round-robin":
				fallthrough
			default:
				backend = pool.GetNextBackendByRoundRobin()
			}
			if backend == nil {
				log.Println("No available backends")
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on no backend error: %v", err)
				}
				return
			}
			if strategyNeedsConns {
				backend.IncrementConnections()
				defer backend.DecrementConnections()
			}
			backendConn, err := net.DialTimeout("tcp", backend.Addr, 3*time.Second)
			if err != nil {
				log.Printf("Failed to connect to backend: %s", backend.Addr)
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on backend dial error: %v", err)
				}
				if strategyNeedsConns {
					backend.DecrementConnections()
				}
				return
			}
			handleProxy(c, backendConn)
		}(client)
	}
}

// NEW: StartMetricsServer starts a separate server for /metrics
func StartMetricsServer(addr string, pool *BackendPool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		// Format and write metrics
		var body string

		// 1. Total active connections
		totalConns := pool.GetTotalConnections()
		body += fmt.Sprintf("total_active_connections %d\n", totalConns)

		// 2. Per-backend stats
		// THE FIX: Change 'i, b' to '_, b'
		for _, b := range pool.backends {
			// Health status (1 for up, 0 for down)
			health := 0
			if b.IsHealthy() {
				health = 1
			}
			body += fmt.Sprintf("backend_health_status{backend=\"%s\"} %d\n", b.Addr, health)

			// Active connections per backend
			conns := b.GetConnections()
			body += fmt.Sprintf("backend_active_connections{backend=\"%s\"} %d\n", b.Addr, conns)
		}

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

// CHANGED: main now starts the metrics server in a goroutine
func main() {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	pool := NewBackendPool(cfg)
	pool.StartHealthChecks()

	// NEW: Start the metrics server
	if cfg.MetricsAddr != "" {
		go StartMetricsServer(cfg.MetricsAddr, pool)
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("Failed to create listener: %v", err)
	}
	var wg sync.WaitGroup
	go func() {
		if err := RunLoadBalancer(cfg, pool, listener, &wg); err != nil {
			log.Printf("Failed to run load balancer: %v", err)
		}
	}()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down... Stopping new connections.")
	if err := listener.Close(); err != nil {
		log.Printf("Warning: failed to close listener: %v", err)
	}
	log.Println("Waiting for all active connections to finish...")
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Println("Graceful shutdown complete.")
	case <-time.After(30 * time.Second):
		log.Println("Shutdown timeout. Forcing exit.")
	}
}
