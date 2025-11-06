package backend

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
)

// RunServer starts the backend http server on the given port.
func RunServer(port, serverID string) (net.Listener, error) {
	log.Printf("Starting backend server %s on port %s\n", serverID, port)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// --- NEW LOGGING ---
		// Read the headers set by the load balancer
		realIP := r.Header.Get("X-Forwarded-For")
		host := r.Header.Get("X-Forwarded-Host")
		proto := r.Header.Get("X-Forwarded-Proto")

		// Log all the new information
		log.Printf(
			"[Server %s] Received request. LB-IP: %s, Client-IP: %s, Host: %s, Proto: %s",
			serverID,
			r.RemoteAddr, // This is the LB's container IP
			realIP,       // This is the original client's IP
			host,
			proto,
		)
		// --- END NEW LOGGING ---

		// Check the error return value.
		if _, err := fmt.Fprintf(w, "Hello from backend server: %s\n", serverID); err != nil {
			log.Printf("Warning: failed to write response: %v", err)
		}
	})

	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return nil, err
	}

	go func() {
		err := http.Serve(listener, handler)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	return listener, nil
}
