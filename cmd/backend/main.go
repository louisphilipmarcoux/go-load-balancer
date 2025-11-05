package main

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
)

// handleProxy now *only* handles the bidirectional copy.
// It's "dumb" and unit-testable.
func handleProxy(client, backend net.Conn) {
	defer client.Close()
	defer backend.Close()

	log.Printf("Proxying traffic for %s to %s", client.RemoteAddr(), backend.RemoteAddr())

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine for client -> backend
	go func() {
		defer wg.Done()
		if _, err := io.Copy(backend, client); err != nil {
			log.Printf("Error copying from client to backend: %v", err)
		}
	}()

	// Goroutine for backend -> client
	go func() {
		defer wg.Done()
		if _, err := io.Copy(client, backend); err != nil {
			log.Printf("Error copying from backend to client: %v", err)
		}
	}()

	wg.Wait()
	log.Printf("Connection for %s closed", client.RemoteAddr())
}

// RunLoadBalancer starts the load balancer and blocks until it's closed.
func RunLoadBalancer(listenAddr, backendAddr string) error {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()

	log.Printf("Load Balancer listening on %s", listenAddr)

	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				log.Println("Listener closed.")
				return nil // Graceful shutdown
			}
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		// This goroutine handles the entire lifecycle for one client
		go func(c net.Conn) {
			// The dialing logic is now here
			backend, err := net.Dial("tcp", backendAddr)
			if err != nil {
				log.Printf("Failed to connect to backend: %v", err)
				c.Close()
				return
			}

			// Call the simple, testable proxy function
			handleProxy(c, backend)
		}(client)
	}
}

// main is the entry point
func main() {
	const backendAddr = "localhost:9001"

	if err := RunLoadBalancer(":8080", backendAddr); err != nil {
		log.Fatalf("Failed to run load balancer: %v", err)
	}
}
