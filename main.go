package main

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
)

type Backend struct {
	Addr string
}

type BackendPool struct {
	backends []*Backend
	// NEW: A counter to track the next backend to use
	current uint64
	lock    sync.Mutex
}

// CHANGED: This method now implements Round Robin
func (p *BackendPool) GetNextBackend() *Backend {
	p.lock.Lock()
	defer p.lock.Unlock()

	// Increment the counter. We use uint64 to prevent overflow
	// and let it wrap around naturally.
	p.current++

	// Use the modulo operator to get an index within the
	// bounds of the backends slice.
	index := p.current % uint64(len(p.backends))

	// Return the backend at that index
	return p.backends[index]
}

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

func main() {
	pool := &BackendPool{
		backends: []*Backend{
			{Addr: "localhost:9001"},
			{Addr: "localhost:9002"},
			{Addr: "localhost:9003"},
		},
	}

	if err := RunLoadBalancer(":8080", pool); err != nil {
		log.Fatalf("Failed to run load balancer: %v", err)
	}
}
