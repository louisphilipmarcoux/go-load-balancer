package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http" // NEW
	"strings"
	"sync"

	"github.com/patrickmn/go-cache"
	"github.com/sony/gobreaker"
)

// ... (Route struct, Matches, LoadBalancer struct, CacheItem, NewLoadBalancer - no changes) ...
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
	lb.ReloadConfig(cfg) // Calls the safe, locking version
	return lb
}

// ... (buildRoutes - no changes) ...
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
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			backend := pool.GetNextBackend(r)
			if backend == nil {
				log.Printf("No healthy, non-tripped backend found for: %s%s", r.Host, r.URL.Path)
				http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
				return
			}
			strategyNeedsConns := pool.strategy == "least-connections" || pool.strategy == "weighted-least-connections"
			if strategyNeedsConns {
				backend.IncrementConnections()
				defer func() {
					backend.DecrementConnections()
				}()
			}
			recorder := &CachingRecorder{
				ResponseWriter: w,
				Status:         http.StatusOK,
				Body:           new(bytes.Buffer),
			}
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
				log.Printf("L7 Proxy: %s%s -> %s", r.Host, r.URL.Path, backend.Addr)
				backend.proxy.ServeHTTP(recorder, r)
			}
			if requestErr != nil {
				if errors.Is(requestErr, gobreaker.ErrOpenState) || errors.Is(requestErr, gobreaker.ErrTooManyRequests) {
					log.Printf("Request blocked for %s (CB: %s)", backend.Addr, requestErr)
					http.Error(w, "Service unavailable (Circuit OPEN)", http.StatusServiceUnavailable)
				} else {
					log.Printf("Failure recorded for %s: %v", backend.Addr, requestErr)
					if !recorder.wroteHeader {
						http.Error(w, "Bad Gateway", http.StatusBadGateway)
					}
				}
				return
			}
			if cacheEnabled && r.Method == http.MethodGet && recorder.Status < 500 {
				item := CacheItem{
					Status: recorder.Status,
					Header: recorder.Header().Clone(),
					Body:   recorder.Body.Bytes(),
				}
				log.Printf("Caching response for: %s (Size: %d bytes)", cacheKey, len(item.Body))
				lb.cache.Set(cacheKey, item, cache.DefaultExpiration)
			}
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

// ... (getRoutes, ServeHTTP - no changes) ...
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

// --- THIS IS THE DEADLOCK FIX ---

// reloadConfig_unsafe atomically swaps the config without locking.
func (lb *LoadBalancer) reloadConfig_unsafe(cfg *Config) {
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

	// We are already inside a lock, so we just set the fields
	lb.cfg = cfg
	lb.routes = newRoutes
	lb.rl = newRl
	lb.cache = newCache

	log.Println("Configuration successfully reloaded (unsafe).")
}

// ReloadConfig is the public, thread-safe method for reloading.
func (lb *LoadBalancer) ReloadConfig(cfg *Config) {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	lb.reloadConfig_unsafe(cfg)
	log.Println("Configuration successfully reloaded (safe).")
}

// --- Admin API Helper Methods (NOW FIXED) ---

// getRouteByIndex finds a route by its index in the config
func (lb *LoadBalancer) getRouteByIndex(index int) (*RouteConfig, error) {
	if index < 0 || index >= len(lb.cfg.Routes) {
		return nil, fmt.Errorf("route index %d out of bounds", index)
	}
	return lb.cfg.Routes[index], nil
}

// AddRoute adds a new route to the config and reloads
func (lb *LoadBalancer) AddRoute(newRoute *RouteConfig) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()

	for _, route := range lb.cfg.Routes {
		if route.Path == newRoute.Path && route.Host == newRoute.Host {
			return fmt.Errorf("route with path '%s' and host '%s' already exists", newRoute.Path, newRoute.Host)
		}
	}
	lb.cfg.Routes = append(lb.cfg.Routes, newRoute)

	lb.reloadConfig_unsafe(lb.cfg) // Call the unsafe version
	return nil
}

// DeleteRoute removes a route from the config and reloads
func (lb *LoadBalancer) DeleteRoute(index int) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()

	if _, err := lb.getRouteByIndex(index); err != nil {
		return err
	}

	// Remove element at index
	lb.cfg.Routes = append(lb.cfg.Routes[:index], lb.cfg.Routes[index+1:]...)

	lb.reloadConfig_unsafe(lb.cfg) // Call the unsafe version
	return nil
}

// AddBackendToRoute adds a backend to a specific route and reloads
func (lb *LoadBalancer) AddBackendToRoute(index int, newBackend *BackendConfig) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()

	route, err := lb.getRouteByIndex(index)
	if err != nil {
		return err
	}

	for _, be := range route.Backends {
		if be.Addr == newBackend.Addr {
			return fmt.Errorf("backend '%s' already exists in route %d", newBackend.Addr, index)
		}
	}
	if newBackend.Weight == 0 {
		newBackend.Weight = 1
	}
	route.Backends = append(route.Backends, newBackend)

	lb.reloadConfig_unsafe(lb.cfg) // Call the unsafe version
	return nil
}

// DeleteBackendFromRoute removes a backend from a specific route and reloads
func (lb *LoadBalancer) DeleteBackendFromRoute(index int, host string) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()

	route, err := lb.getRouteByIndex(index)
	if err != nil {
		return err
	}

	found := false
	newBackends := make([]*BackendConfig, 0)
	for _, be := range route.Backends {
		if be.Addr == host {
			found = true
			continue
		}
		newBackends = append(newBackends, be)
	}
	if !found {
		return fmt.Errorf("backend '%s' not found in route %d", host, index)
	}
	route.Backends = newBackends

	lb.reloadConfig_unsafe(lb.cfg) // Call the unsafe version
	return nil
}

// UpdateBackendWeightInRoute updates a backend's weight in a route and reloads
func (lb *LoadBalancer) UpdateBackendWeightInRoute(index int, host string, weight int) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()

	route, err := lb.getRouteByIndex(index)
	if err != nil {
		return err
	}
	if weight <= 0 {
		return fmt.Errorf("weight must be > 0")
	}
	found := false
	for _, be := range route.Backends {
		if be.Addr == host {
			be.Weight = weight
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("backend '%s' not found in route %d", host, index)
	}

	lb.reloadConfig_unsafe(lb.cfg) // Call the unsafe version
	return nil
}
