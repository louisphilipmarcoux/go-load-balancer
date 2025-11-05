package backend

import (
	"errors" // <-- ADD THIS IMPORT
	"fmt"
	"log"
	"net" // <-- ADD THIS IMPORT
	"net/http"
)

// RunServer starts the backend http server on the given port.
// Notice it's an EXPORTED function (starts with capital 'R').
func RunServer(port, serverID string) (net.Listener, error) {
	log.Printf("Starting backend server %s on port %s\n", serverID, port)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[Server %s] Received request from %s", serverID, r.RemoteAddr)
		fmt.Fprintf(w, "Hello from backend server: %s\n", serverID)
	})

	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return nil, err
	}

	// Start serving in a goroutine so this function can return
	go func() {
		err := http.Serve(listener, nil)

		// UPDATED: This error is expected when we call listener.Close() in our test.
		// We check for it and exit gracefully instead of calling log.Fatalf.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	return listener, nil
}
