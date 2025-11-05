package main

import (
	"errors"
	"fmt" // NEW: Import fmt
	"hash/fnv"
	"io"
	"log"
	"math"
	"net"
	"os" // NEW: Import os
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3" // NEW: Import yaml
)

// --- NEW: Config Structs ---

// Config struct to hold all our settings
type Config struct {
	ListenAddr string           `yaml:"listenAddr"`
	Strategy   string           `yaml:"strategy"`
	Backends   []*BackendConfig `yaml:"backends"`
}

// BackendConfig holds the config for a single backend
type BackendConfig struct {
	Addr string `yaml:"addr"`
	// We'll add 'weight' here in a later stage
}

// LoadConfig reads and parses the config.yaml file
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

// --- END: Config Structs ---

type Backend struct {
	Addr        string
	healthy     bool
	lock        sync.RWMutex
	connections uint64
}

// ... Backend methods (no changes) ...
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

type BackendPool struct {
	backends []*Backend
	// NEW: Store the selected strategy
	strategy string
	// NEW: We need this counter back for Round Robin
	current uint64
}

// NEW: NewBackendPool creates a pool from the config
func NewBackendPool(cfg *Config) *BackendPool {
	backends := make([]*Backend, 0, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		backends = append(backends, &Backend{Addr: bc.Addr})
	}

	return &BackendPool{
		backends: backends,
		strategy: cfg.Strategy,
	}
}

// ... healthCheck (no changes) ...
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

// ... StartHealthChecks (no changes) ...
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

// --- Strategy Implementations ---

// GetNextBackendByIP implements IP Hashing
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

// GetNextBackendByLeastConns implements Least Connections
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

// GetNextBackendByRoundRobin implements Round Robin
func (p *BackendPool) GetNextBackendByRoundRobin() *Backend {
	// We need a Read Lock here, not a full Lock,
	// but let's add a sync.RWMutex to the pool first.
	// For now, let's just use an atomic for the counter.
	// We'll refactor the lock in a bit.

	// Let's re-add the lock to the pool
	// ... (see BackendPool struct)

	// This function is complex with concurrent health checks.
	// Let's simplify and add the lock back.

	// --- REFECTORING BackendPool ---
	// (See above) ... We'll add the lock back.
	// And `current` is already there.

	// This is a new refactor. We'll add a new lock
	// to BackendPool, just for the counter.

	// ... Let's make this simple.

	numBackends := uint64(len(p.backends))
	if numBackends == 0 {
		return nil
	}

	// Loop to find a healthy one
	for i := uint64(0); i < numBackends; i++ {
		// We'll use atomic.AddUint64 to safely increment
		index := atomic.AddUint64(&p.current, 1) % numBackends

		backend := p.backends[index]
		if backend.IsHealthy() {
			return backend
		}
	}
	return nil
}

// ... handleProxy (no changes) ...
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

// CHANGED: Now accepts a *Config
func RunLoadBalancer(cfg *Config, pool *BackendPool) error {
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer func() {
		if err := listener.Close(); err != nil {
			log.Printf("Warning: failed to close listener: %v", err)
		}
	}()

	log.Printf("Load Balancer listening on %s, strategy: %s", cfg.ListenAddr, cfg.Strategy)

	for {
		client, err := listener.Accept()
		if err != nil {
			// ... (error handling, no changes) ...
			if errors.Is(err, net.ErrClosed) {
				log.Println("Listener closed.")
				return nil
			}
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		go func(c net.Conn) {
			// --- NEW: STRATEGY ROUTING ---
			var backend *Backend

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

			case "round-robin":
				backend = pool.GetNextBackendByRoundRobin()

			default:
				// Default to Round Robin
				backend = pool.GetNextBackendByRoundRobin()
			}
			// --- END: STRATEGY ROUTING ---

			if backend == nil {
				log.Println("No available backends")
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on no backend error: %v", err)
				}
				return
			}

			// For "least-connections", we must inc/dec the counter
			if pool.strategy == "least-connections" {
				backend.IncrementConnections()
				defer backend.DecrementConnections()
			}

			backendConn, err := net.Dial("tcp", backend.Addr)
			if err != nil {
				log.Printf("Failed to connect to backend: %s", backend.Addr)
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on backend dial error: %v", err)
				}
				if pool.strategy == "least-connections" {
					backend.DecrementConnections() // Must decrement on dial error
				}
				return
			}

			handleProxy(c, backendConn)
		}(client)
	}
}

// CHANGED: Load config and start
func main() {
	// 1. Load configuration
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// 2. Create the backend pool from the config
	pool := NewBackendPool(cfg)

	// 3. Start health checks
	pool.StartHealthChecks()

	// 4. Run the load balancer
	if err := RunLoadBalancer(cfg, pool); err != nil {
		log.Fatalf("Failed to run load balancer: %v", err)
	}
}
