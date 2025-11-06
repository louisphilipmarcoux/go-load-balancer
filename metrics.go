package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sony/gobreaker"
)

// CachingRecorder captures Status, Headers, and Body
type CachingRecorder struct {
	http.ResponseWriter
	Status      int
	Body        *bytes.Buffer
	wroteHeader bool // Did we write the header yet?
}

func (r *CachingRecorder) WriteHeader(status int) {
	r.Status = status
	r.wroteHeader = true
	// Don't write to underlying writer yet!
	// We'll do that at the very end.
}

func (r *CachingRecorder) Write(b []byte) (int, error) {
	// Write to our buffer
	return r.Body.Write(b)
}

// StartMetricsServer starts the /metrics endpoint
func StartMetricsServer(addr string, lb *LoadBalancer) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		lb.lock.RLock()
		routes := lb.routes
		cache := lb.cache
		lb.lock.RUnlock()

		var body string
		var totalConns uint64

		for _, route := range routes {
			totalConns += route.pool.GetTotalConnections()
			for _, b := range route.pool.backends {
				health := 0
				if b.IsHealthy() {
					health = 1
				}
				label := fmt.Sprintf("backend=\"%s\", route_path=\"%s\", route_host=\"%s\"",
					b.Addr, route.config.Path, route.config.Host)

				body += fmt.Sprintf("backend_health_status{%s} %d\n", label, health)

				// --- THIS IS THE FIX ---
				conns := b.GetConnections()
				// --- END OF FIX ---

				body += fmt.Sprintf("backend_active_connections{%s} %d\n", label, conns)

				if b.cb != nil {
					state := b.cb.State()
					var stateNum int
					switch state {
					case gobreaker.StateClosed:
						stateNum = 0
					case gobreaker.StateHalfOpen:
						stateNum = 1
					case gobreaker.StateOpen:
						stateNum = 2
					}
					body += fmt.Sprintf("backend_circuit_state{%s} %d\n", label, stateNum)
				}

				body += "\n" // Add a newline to separate each backend's metrics
			}
		}

		body += "\n" // Add a newline to separate summary stats
		body += fmt.Sprintf("total_active_connections %d\n", totalConns)
		if cache != nil {
			body += fmt.Sprintf("cache_item_count %d\n", cache.ItemCount())
		}

		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte(body)); err != nil {
			log.Printf("Warning: failed to write metrics response: %v", err)
		}
	})

	log.Printf("Metrics server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("Metrics server failed: %v", err)
	}
}
