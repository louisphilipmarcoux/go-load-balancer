package main

import (
	"errors"
	"io"
	"log"
	"math" // NEW: Import math
	"net"
	"sync"
	"sync/atomic" // NEW: Import atomic
	"time"
)

type Backend struct {
	Addr    string
	healthy bool
	lock    sync.RWMutex
	// NEW: A thread-safe counter for active connections
	connections uint64
}

// ... IsHealthy and SetHealth methods (no changes) ...
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

// NEW: Atomically increments the connection counter
func (b *Backend) IncrementConnections() {
	atomic.AddUint64(&b.connections, 1)
}

// NEW: Atomically decrements the connection counter
func (b *Backend) DecrementConnections() {
	atomic.AddUint64(&b.connections, ^uint64(0)) // ^uint64(0) is -1 in two's complement
}

// NEW: Atomically gets the current connection count
func (b *Backend) GetConnections() uint64 {
	return atomic.LoadUint64(&b.connections)
}

type BackendPool struct {
	backends []*Backend
	// Note: 'current' is no longer used by GetNextBackend,
	// but we'll leave it for now.
	current uint64
	lock    sync.Mutex
}

// ... healthCheck and StartHealthChecks methods (no changes) ...
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

// CHANGED: This method now implements the Least Connections algorithm
func (p *BackendPool) GetNextBackend() *Backend {
	var bestBackend *Backend
	minConnections := uint64(math.MaxUint64) // Start with the highest possible value

	// We don't need the pool's main lock here, because we are
	// only reading the backends slice (which doesn't change)
	// and the health/connection counters use their own atomic
	// or RWMutex locks.

	for _, backend := range p.backends {
		if !backend.IsHealthy() {
			continue // Skip unhealthy backends
		}

		// Find the backend with the minimum connections
		conns := backend.GetConnections()
		if conns < minConnections {
			minConnections = conns
			bestBackend = backend
		}
	}

	return bestBackend // This will be nil if all backends are down
}

// ... handleProxy function (no changes) ...
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

// CHANGED: We now increment/decrement the connection counter
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
			backend := pool.GetNextBackend()
			if backend == nil {
				log.Println("No available backends")
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on no backend error: %v", err)
				}
				return
			}

			// NEW: Increment counter *before* proxying
			backend.IncrementConnections()
			// NEW: Decrement counter *after* proxying is finished
			// This happens when handleProxy returns (which is when the connection is closed)
			defer backend.DecrementConnections()

			backendConn, err := net.Dial("tcp", backend.Addr)
			if err != nil {
				log.Printf("Failed to connect to backend: %v", err)
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on backend dial error: %v", err)
				}
				// We must also decrement here, since the connection failed
				backend.DecrementConnections()
				return
			}

			handleProxy(c, backendConn)
		}(client)
	}
}

// ... main function (no changes) ...
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
