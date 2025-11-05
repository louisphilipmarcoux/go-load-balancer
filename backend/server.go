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

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[Server %s] Received request from %s", serverID, r.RemoteAddr)
		// Check the error return value.
		if _, err := fmt.Fprintf(w, "Hello from backend server: %s\n", serverID); err != nil {
			log.Printf("Warning: failed to write response: %v", err)
		}
	})

	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return nil, err
	}

	// Start serving in a goroutine so this function can return
	go func() {
		err := http.Serve(listener, nil)

		// We expect a net.ErrClosed when the listener is closed,
		// We check for it and exit gracefully instead of calling log.Fatalf.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	return listener, nil
}
