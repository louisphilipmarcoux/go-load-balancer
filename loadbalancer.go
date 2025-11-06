package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	// "net/http/httputil" // No longer needed here
	// "net/url" // No longer needed here
	"strings"
	"sync"

	"github.com/sony/gobreaker"
)

// ... (Route struct - no changes) ...
type Route struct {
	config  *RouteConfig
	pool    *BackendPool
	handler http.Handler
}

func (rt *Route) Matches(r *http.Request) bool {
	if rt.config.Host != "" && rt.config.Host != r.Host {
		return false
	}
	if rt.config.Path != "" && !strings.HasPrefix(r.URL.Path, rt.config.Path) {
		return false
	}
	for key, value := range rt.config.Headers {
		if r.Header.Get(key) != value {
			return false
		}
	}
	return true
}

// ... (LoadBalancer struct - no changes) ...
type LoadBalancer struct {
	cfg    *Config
	routes []*Route
	rl     *RateLimiter
	lock   sync.RWMutex
}

func NewLoadBalancer(cfg *Config) *LoadBalancer {
	lb := &LoadBalancer{}
	lb.ReloadConfig(cfg)
	return lb
}

// CHANGED: buildRoutes now passes connection pool config
func (lb *LoadBalancer) buildRoutes(cfg *Config) []*Route {
	routes := make([]*Route, 0, len(cfg.Routes))

	for _, routeCfg := range cfg.Routes {
		// NEW: Pass connection pool and circuit breaker configs
		pool := NewBackendPool(
			routeCfg.Strategy,
			routeCfg.Backends,
			cfg.CircuitBreaker,
			cfg.ConnectionPool, // Pass the new config
		)
		pool.StartHealthChecks()

		// Create the reverse proxy handler for this pool
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			backend := pool.GetNextBackend(r)
			if backend == nil {
				log.Printf("No healthy, non-tripped backend found for: %s%s", r.Host, r.URL.Path)
				http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
				return
			}

			// ... (Connection counting - no changes) ...
			strategyNeedsConns := pool.strategy == "least-connections" ||
				pool.strategy == "weighted-least-connections"
			if strategyNeedsConns {
				backend.IncrementConnections()
				defer func() {
					backend.DecrementConnections()
				}()
			}

			// GONE: All proxy/URL creation logic is gone from here

			// --- Circuit Breaker Logic ---
			if backend.cb != nil {
				_, err := backend.cb.Execute(func() (interface{}, error) {
					log.Printf("L7 Proxy: %s%s -> %s (CB: %s)", r.Host, r.URL.Path, backend.Addr, backend.cb.State())

					recorder := &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}

					// NEW: Set the error handler *just before* the request
					// This ensures the recorder is in scope
					backend.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
						// This is a connection failure
						// We can't write to 'w' here, as the recorder won't capture it.
						// The error return will be handled by the circuit breaker logic.
						log.Printf("Backend connection error for %s: %v", backend.Addr, err)
					}

					// Use the pre-built proxy
					backend.proxy.ServeHTTP(recorder, r)

					if recorder.Status >= http.StatusInternalServerError {
						return nil, fmt.Errorf("backend %s returned status %d", backend.Addr, recorder.Status)
					}
					return nil, nil
				})

				if err != nil {
					if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
						log.Printf("Request blocked for %s (CB: %s)", backend.Addr, backend.cb.State())
						http.Error(w, "Service unavailable (Circuit OPEN)", http.StatusServiceUnavailable)
					} else {
						// Log the 5xx error or connection error
						log.Printf("Failure recorded for %s: %v", backend.Addr, err)
						// If the response wasn't already written (e.g., connection error),
						// send a 502.
						if w.Header().Get("Content-Type") == "" {
							http.Error(w, "Bad Gateway", http.StatusBadGateway)
						}
					}
				}
			} else {
				// No circuit breaker
				log.Printf("L7 Proxy: %s%s -> %s", r.Host, r.URL.Path, backend.Addr)
				// Use the pre-built proxy
				backend.proxy.ServeHTTP(w, r)
			}
		})

		routes = append(routes, &Route{
			config:  routeCfg,
			pool:    pool,
			handler: handler,
		})
	}
	return routes
}

// ... (getRoutes, ServeHTTP, ReloadConfig - no changes) ...
func (lb *LoadBalancer) getRoutes() []*Route {
	lb.lock.RLock()
	defer lb.lock.RUnlock()
	return lb.routes
}
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lb.lock.RLock()
	limiterEnabled := lb.rl != nil
	lb.lock.RUnlock()

	if limiterEnabled {
		clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			log.Printf("Failed to get client IP for rate limiting: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		ipLimiter := lb.rl.GetLimiter(clientIP)
		if !ipLimiter.Allow() {
			log.Printf("Rate limit exceeded for IP: %s", clientIP)
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}
	}
	currentRoutes := lb.getRoutes()
	for _, route := range currentRoutes {
		if route.Matches(r) {
			route.handler.ServeHTTP(w, r)
			return
		}
	}
	log.Printf("No route found for: %s%s", r.Host, r.URL.Path)
	http.NotFound(w, r)
}
func (lb *LoadBalancer) ReloadConfig(cfg *Config) {
	newRoutes := lb.buildRoutes(cfg)
	var newRl *RateLimiter
	if cfg.RateLimit != nil && cfg.RateLimit.Enabled {
		newRl = NewRateLimiter(cfg.RateLimit)
	}

	lb.lock.Lock()
	lb.cfg = cfg
	lb.routes = newRoutes
	lb.rl = newRl
	lb.lock.Unlock()

	log.Println("Configuration successfully reloaded.")
}
