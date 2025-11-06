package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/sony/gobreaker"
)

// StatusRecorder captures the HTTP status code
type StatusRecorder struct {
	http.ResponseWriter
	Status int
}

func (r *StatusRecorder) WriteHeader(status int) {
	r.Status = status
	r.ResponseWriter.WriteHeader(status)
}

// StartMetricsServer starts the /metrics endpoint
func StartMetricsServer(addr string, lb *LoadBalancer) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		lb.lock.RLock()
		routes := lb.routes
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
				// Use path and host as labels
				label := fmt.Sprintf("backend=\"%s\", route_path=\"%s\", route_host=\"%s\"",
					b.Addr, route.config.Path, route.config.Host)

				body += fmt.Sprintf("backend_health_status{%s} %d\n", label, health)
				conns := b.GetConnections()
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
			}
		}
		body += fmt.Sprintf("total_active_connections %d\n", totalConns)

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
