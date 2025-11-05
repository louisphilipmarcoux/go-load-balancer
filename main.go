package main

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time" // NEW: Import time
)

// NEW: Backend struct is now more complex
type Backend struct {
	Addr    string
	healthy bool
	// RWMutex allows many goroutines to Read (IsHealthy)
	// but only one to Write (SetHealth)
	lock sync.RWMutex
}

// NEW: IsHealthy checks the health status in a thread-safe way
func (b *Backend) IsHealthy() bool {
	b.lock.RLock() // Grab a Read-lock
	defer b.lock.RUnlock()
	return b.healthy
}

// NEW: SetHealth updates the health status in a thread-safe way
func (b *Backend) SetHealth(healthy bool) {
	b.lock.Lock() // Grab a Write-lock
	defer b.lock.Unlock()
	b.healthy = healthy
}

type BackendPool struct {
	backends []*Backend
	current  uint64
	lock     sync.Mutex
}

// NEW: healthCheck is a helper that runs for each backend
func (p *BackendPool) healthCheck(b *Backend) {
	// Attempt to connect with a short timeout
	conn, err := net.DialTimeout("tcp", b.Addr, 2*time.Second)
	if err != nil {
		// Connection failed, mark as unhealthy
		if b.IsHealthy() { // Only log if the state changes
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

	// Connection successful, mark as healthy
	if !b.IsHealthy() { // Only log if the state changes
		log.Printf("Backend %s is UP", b.Addr)
		b.SetHealth(true)
	}
}

// NEW: StartHealthChecks runs a continuous health check loop
func (p *BackendPool) StartHealthChecks() {
	log.Println("Starting health checks...")
	// Run an initial check on all backends
	for _, b := range p.backends {
		go p.healthCheck(b) // Run in a goroutine so they check in parallel
	}

	// Run checks every 5 seconds in a new goroutine
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

// GetNextBackend now just does round-robin.
// It does NOT check for health yet (that's Stage 6).
func (p *BackendPool) GetNextBackend() *Backend {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.current++
	index := p.current % uint64(len(p.backends))
	return p.backends[index]
}

func handleProxy(client, backend net.Conn) {
	// ... (no changes to this function)
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

func RunLoadBalancer(listenAddr string, pool *BackendPool) error {
	// ... (no changes to this function)
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

func main() {
	pool := &BackendPool{
		backends: []*Backend{
			{Addr: "localhost:9001"},
			{Addr: "localhost:9002"},
			{Addr: "localhost:9003"},
		},
	}

	// NEW: Start the health check goroutine
	pool.StartHealthChecks()

	if err := RunLoadBalancer(":8080", pool); err != nil {
		log.Fatalf("Failed to run load balancer: %v", err)
	}
}
