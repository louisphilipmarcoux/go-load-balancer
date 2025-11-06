package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/patrickmn/go-cache"
	"github.com/sony/gobreaker"
)

// ... (Route struct, Matches - no changes) ...
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

// ... (LoadBalancer struct, CacheItem, NewLoadBalancer - no changes) ...
type LoadBalancer struct {
	cfg    *Config
	routes []*Route
	rl     *RateLimiter
	cache  *cache.Cache
	lock   sync.RWMutex
}
type CacheItem struct {
	Status int
	Header http.Header
	Body   []byte
}

func NewLoadBalancer(cfg *Config) *LoadBalancer {
	lb := &LoadBalancer{}
	lb.ReloadConfig(cfg)
	return lb
}

// buildRoutes - simplified handler logic
func (lb *LoadBalancer) buildRoutes(cfg *Config) []*Route {
	routes := make([]*Route, 0, len(cfg.Routes))

	for _, routeCfg := range cfg.Routes {
		pool := NewBackendPool(
			routeCfg.Strategy,
			routeCfg.Backends,
			cfg.CircuitBreaker,
			cfg.ConnectionPool,
		)
		pool.StartHealthChecks()

		// Create the reverse proxy handler for this pool
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// --- CACHE CHECK (STAGE 21) ---
			lb.lock.RLock()
			cacheEnabled := lb.cache != nil
			lb.lock.RUnlock()
			var cacheKey string

			if cacheEnabled && r.Method == http.MethodGet {
				cacheKey = r.Host + r.URL.Path
				if item, found := lb.cache.Get(cacheKey); found {
					log.Printf("Cache HIT for: %s", cacheKey)
					cachedItem := item.(CacheItem)
					for key, values := range cachedItem.Header {
						for _, value := range values {
							w.Header().Add(key, value)
						}
					}
					w.WriteHeader(cachedItem.Status)
					if _, err := w.Write(cachedItem.Body); err != nil {
						log.Printf("Error writing cached response: %v", err)
					}
					return
				}
				log.Printf("Cache MISS for: %s", cacheKey)
			}
			// --- END CACHE CHECK ---

			backend := pool.GetNextBackend(r)
			if backend == nil {
				log.Printf("No healthy, non-tripped backend found for: %s%s", r.Host, r.URL.Path)
				http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
				return
			}

			// ... (Connection counting - no changes) ...
			strategyNeedsConns := pool.strategy == "least-connections" || pool.strategy == "weighted-least-connections"
			if strategyNeedsConns {
				backend.IncrementConnections()
				defer func() {
					backend.DecrementConnections()
				}()
			}

			// We need a recorder to capture status/body for CB and Caching
			recorder := &CachingRecorder{
				ResponseWriter: w,
				Status:         http.StatusOK,
				Body:           new(bytes.Buffer),
			}

			// --- Circuit Breaker / Proxy Logic ---
			var requestErr error
			if backend.cb != nil {
				_, requestErr = backend.cb.Execute(func() (interface{}, error) {
					log.Printf("L7 Proxy: %s%s -> %s (CB: %s)", r.Host, r.URL.Path, backend.Addr, backend.cb.State())
					backend.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
						log.Printf("Backend connection error for %s: %v", backend.Addr, err)
					}
					backend.proxy.ServeHTTP(recorder, r)
					if recorder.Status >= http.StatusInternalServerError {
						return nil, fmt.Errorf("backend %s returned status %d", backend.Addr, recorder.Status)
					}
					return nil, nil
				})
			} else {
				// No circuit breaker, just proxy
				log.Printf("L7 Proxy: %s%s -> %s", r.Host, r.URL.Path, backend.Addr)
				backend.proxy.ServeHTTP(recorder, r)
			}

			// --- Post-Request Processing ---
			if requestErr != nil {
				// Circuit breaker logic
				if errors.Is(requestErr, gobreaker.ErrOpenState) || errors.Is(requestErr, gobreaker.ErrTooManyRequests) {
					log.Printf("Request blocked for %s (CB: %s)", backend.Addr, requestErr)
					http.Error(w, "Service unavailable (Circuit OPEN)", http.StatusServiceUnavailable)
				} else {
					log.Printf("Failure recorded for %s: %v", backend.Addr, requestErr)
					if !recorder.wroteHeader {
						http.Error(w, "Bad Gateway", http.StatusBadGateway)
					}
				}
				return // Don't cache failures
			}

			// --- Caching Logic ---
			// If we got here, request was successful. Check if we should cache.
			if cacheEnabled && r.Method == http.MethodGet && recorder.Status < 500 {
				item := CacheItem{
					Status: recorder.Status,
					Header: recorder.Header().Clone(), // Get headers from the recorder
					Body:   recorder.Body.Bytes(),
				}
				log.Printf("Caching response for: %s (Size: %d bytes)", cacheKey, len(item.Body))
				lb.cache.Set(cacheKey, item, cache.DefaultExpiration)
			}

			// If recorder didn't write, we must.
			if !recorder.wroteHeader {
				recorder.ResponseWriter.WriteHeader(recorder.Status)
			}
			if _, err := recorder.ResponseWriter.Write(recorder.Body.Bytes()); err != nil {
				log.Printf("Error writing final response: %v", err)
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
	var newCache *cache.Cache
	if cfg.Cache != nil && cfg.Cache.Enabled {
		newCache = cache.New(cfg.Cache.DefaultExpiration, cfg.Cache.CleanupInterval)
		log.Printf("Cache enabled with expiration %s", cfg.Cache.DefaultExpiration)
	}

	lb.lock.Lock()
	lb.cfg = cfg
	lb.routes = newRoutes
	lb.rl = newRl
	lb.cache = newCache
	lb.lock.Unlock()

	log.Println("Configuration successfully reloaded.")
}
