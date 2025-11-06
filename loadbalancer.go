package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/sony/gobreaker"
)

// Route holds the compiled logic for a single route
type Route struct {
	config  *RouteConfig
	pool    *BackendPool
	handler http.Handler
}

// Matches checks if a request matches this route's rules
func (rt *Route) Matches(r *http.Request) bool {
	// 1. Match Host
	if rt.config.Host != "" && rt.config.Host != r.Host {
		return false
	}

	// 2. Match Path
	if rt.config.Path != "" && !strings.HasPrefix(r.URL.Path, rt.config.Path) {
		return false
	}

	// 3. Match Headers
	for key, value := range rt.config.Headers {
		if r.Header.Get(key) != value {
			return false
		}
	}

	return true
}

// LoadBalancer holds the state for the entire server
type LoadBalancer struct {
	cfg    *Config
	routes []*Route
	rl     *RateLimiter
	lock   sync.RWMutex
}

// NewLoadBalancer creates a new LoadBalancer
func NewLoadBalancer(cfg *Config) *LoadBalancer {
	lb := &LoadBalancer{}
	lb.ReloadConfig(cfg) // Use ReloadConfig to set initial state
	return lb
}

// buildRoutes creates the http.Handler for each route
func (lb *LoadBalancer) buildRoutes(cfg *Config) []*Route {
	routes := make([]*Route, 0, len(cfg.Routes))

	for _, routeCfg := range cfg.Routes {
		pool := NewBackendPool(routeCfg.Strategy, routeCfg.Backends, cfg.CircuitBreaker)
		pool.StartHealthChecks()

		// Create the reverse proxy handler for this pool
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			backend := pool.GetNextBackend(r)
			if backend == nil {
				log.Printf("No healthy, non-tripped backend found for: %s%s", r.Host, r.URL.Path)
				http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
				return
			}

			// Connection counting
			strategyNeedsConns := pool.strategy == "least-connections" ||
				pool.strategy == "weighted-least-connections"
			if strategyNeedsConns {
				backend.IncrementConnections()
				defer func() {
					backend.DecrementConnections()
				}()
			}

			targetUrl, err := url.Parse("http://" + backend.Addr)
			if err != nil {
				log.Printf("Failed to parse backend URL: %v", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			proxy := httputil.NewSingleHostReverseProxy(targetUrl)

			// Circuit Breaker Logic
			if backend.cb != nil {
				_, err := backend.cb.Execute(func() (interface{}, error) {
					log.Printf("L7 Proxy: %s%s -> %s (CB: %s)", r.Host, r.URL.Path, backend.Addr, backend.cb.State())

					recorder := &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
					proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
						// This is a connection failure
					}

					proxy.ServeHTTP(recorder, r)

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
						log.Printf("Failure recorded for %s: %v", backend.Addr, err)
					}
				}
			} else {
				// No circuit breaker
				log.Printf("L7 Proxy: %s%s -> %s", r.Host, r.URL.Path, backend.Addr)
				proxy.ServeHTTP(w, r)
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

// getRoutes atomically gets the current routes
func (lb *LoadBalancer) getRoutes() []*Route {
	lb.lock.RLock()
	defer lb.lock.RUnlock()
	return lb.routes
}

// ServeHTTP is the main entry point for all requests
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// --- RATE LIMITING LOGIC ---
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
	// --- END RATE LIMITING LOGIC ---

	// --- STAGE 19: ADVANCED ROUTING ---
	currentRoutes := lb.getRoutes()
	for _, route := range currentRoutes {
		if route.Matches(r) {
			route.handler.ServeHTTP(w, r)
			return
		}
	}

	// No route matched
	log.Printf("No route found for: %s%s", r.Host, r.URL.Path)
	http.NotFound(w, r)
}

// ReloadConfig atomically swaps the entire configuration
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
