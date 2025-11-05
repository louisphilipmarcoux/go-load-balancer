package main

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

type Backend struct {
	Addr    string
	healthy bool
	lock    sync.RWMutex
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

type BackendPool struct {
	backends []*Backend
	current  uint64
	lock     sync.Mutex
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

// CHANGED: This method now skips unhealthy backends
func (p *BackendPool) GetNextBackend() *Backend {
	p.lock.Lock()
	defer p.lock.Unlock()

	// Get the total number of backends
	numBackends := uint64(len(p.backends))
	if numBackends == 0 {
		return nil // No backends in the pool
	}

	// Loop up to `numBackends` times to find a healthy one
	// This prevents an infinite loop if all backends are down
	for i := uint64(0); i < numBackends; i++ {
		// Increment and wrap the counter
		p.current++
		index := p.current % numBackends

		// Get the backend at that index
		backend := p.backends[index]

		// NEW: Check if this backend is healthy
		if backend.IsHealthy() {
			return backend // Found one, return it
		}
	}

	// No healthy backends were found
	return nil
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

// ... RunLoadBalancer function (no changes) ...
// The 'if backend == nil' check we added in Stage 3
// now handles the case where all backends are down!
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

			backendConn, err := net.Dial("tcp", backend.Addr)
			if err != nil {
				log.Printf("Failed to connect to backend: %v", err)
				if err := c.Close(); err != nil {
					log.Printf("Warning: failed to close client connection on backend dial error: %v", err)
				}
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
