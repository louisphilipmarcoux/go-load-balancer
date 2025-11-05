package main

import (
	"errors"
	"hash/fnv" // NEW: Import hash/fnv
	"io"
	"log"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	Addr        string
	healthy     bool
	lock        sync.RWMutex
	connections uint64
}

// ... IsHealthy, SetHealth, IncrementConnections, DecrementConnections, GetConnections (no changes) ...
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
}

// ... healthCheck, StartHealthChecks (no changes) ...
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

// GetNextBackend now just selects the least busy backend.
// We'll add a *new* method for IP Hashing.
func (p *BackendPool) GetNextBackend() *Backend {
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

// NEW: GetNextBackendByIP implements IP Hashing
func (p *BackendPool) GetNextBackendByIP(ip string) *Backend {
	// First, collect all healthy backends
	healthyBackends := make([]*Backend, 0)
	for _, backend := range p.backends {
		if backend.IsHealthy() {
			healthyBackends = append(healthyBackends, backend)
		}
	}

	if len(healthyBackends) == 0 {
		return nil
	}

	// Create a new FNV-1a hash
	h := fnv.New32a()
	// Write the IP string to the hash
	h.Write([]byte(ip))
	// Get the 32-bit hash value
	hashValue := h.Sum32()

	// Use the hash value to pick a backend
	index := int(hashValue) % len(healthyBackends)

	return healthyBackends[index]
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

// CHANGED: We now get the client IP and use GetNextBackendByIP
func RunLoadBalancer(listenAddr string, pool *BackendPool) error {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer func() {
		if err := listener.Close(); err != nil {
			log.Printf("Warning: failed to close listener: %v", err)
		}
	}()

	log.Printf("Load Balancer listening on %s", listenAddr)

	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				log.Println("Listener closed.")
				return nil
			}
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		go func(c net.Conn) {
			// NEW: Get the client's IP address
			clientIP, _, err := net.SplitHostPort(c.RemoteAddr().String())
			if err != nil {
				log.Printf("Failed to get client IP: %v", err)
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on IP error: %v", err)
				}
				return
			}

			// CHANGED: Get backend by IP hash
			backend := pool.GetNextBackendByIP(clientIP)
			if backend == nil {
				log.Println("No available backends")
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on no backend error: %v", err)
				}
				return
			}

			// We are no longer using Least Connections, so remove these
			// backend.IncrementConnections()
			// defer backend.DecrementConnections()

			backendConn, err := net.Dial("tcp", backend.Addr)
			if err != nil {
				log.Printf("Failed to connect to backend: %s", backend.Addr)
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on backend dial error: %v", err)
				}
				// backend.DecrementConnections() // No longer used
				return
			}

			handleProxy(c, backendConn)
		}(client)
	}
}

// ... main (no changes) ...
func main() {
	pool := &BackendPool{
		backends: []*Backend{
			{Addr: "localhost:9001"},
			{Addr: "localhost:9002"},
			{Addr: "localhost:9003"},
		},
	}
	pool.StartHealthChecks()

	if err := RunLoadBalancer(":8080", pool); err != nil {
		log.Fatalf("Failed to run load balancer: %v", err)
	}
}
