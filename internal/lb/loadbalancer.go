package lb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http" // NEW
	"strings"
	"sync"

	"github.com/patrickmn/go-cache"
	"github.com/sony/gobreaker"
)

// A private key type to prevent context collisions
type contextKey string

const loggerKey = contextKey("logger")

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
	consul *ConsulClient
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

func (lb *LoadBalancer) reloadConfig_unsafe(cfg *Config) {
	// Re-create consul client on reload (in case addr changed)
	if cfg.Consul != nil && cfg.Consul.Addr != "" {
		consulClient, err := NewConsulClient(cfg.Consul.Addr)
		if err != nil {
			slog.Error("Failed to re-initialize Consul client", "error", err)
			lb.consul = nil
		} else {
			lb.consul = consulClient
		}
	} else {
		lb.consul = nil
	}

	newRoutes := lb.buildRoutes(cfg)
	var newRl *RateLimiter
	if cfg.RateLimit != nil && cfg.RateLimit.Enabled {
		newRl = NewRateLimiter(cfg)
	}
	var newCache *cache.Cache
	if cfg.Cache != nil && cfg.Cache.Enabled {
		newCache = cache.New(cfg.Cache.DefaultExpiration, cfg.Cache.CleanupInterval)
		slog.Info("Cache enabled", "expiration", cfg.Cache.DefaultExpiration)
	}
	lb.cfg = cfg
	lb.routes = newRoutes
	lb.rl = newRl
	lb.cache = newCache
	slog.Info("Configuration successfully reloaded (unsafe)")
}

// Add this method back to internal/lb/loadbalancer.go

func (lb *LoadBalancer) ReloadConfig(cfg *Config) {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	lb.reloadConfig_unsafe(cfg)
	slog.Info("Configuration successfully reloaded (safe)")
}

func NewLoadBalancer(cfg *Config) *LoadBalancer {
	lb := &LoadBalancer{}

	// Create the Consul client *first*
	if cfg.Consul != nil && cfg.Consul.Addr != "" {
		consulClient, err := NewConsulClient(cfg.Consul.Addr)
		if err != nil {
			slog.Error("Failed to initialize Consul client, service discovery will fail", "error", err)
		} else {
			// Only assign the client on success
			lb.consul = consulClient
		}
	}

	// --- THIS IS THE CHANGE ---
	// Create the rate limiter
	if cfg.RateLimit != nil && cfg.RateLimit.Enabled {
		lb.rl = NewRateLimiter(cfg) // NewRateLimiter now takes the whole config
		if lb.rl == nil {
			slog.Warn("Rate limiter initialization failed, disabling.")
		}
	}
	// Create the cache
	if cfg.Cache != nil && cfg.Cache.Enabled {
		lb.cache = cache.New(cfg.Cache.DefaultExpiration, cfg.Cache.CleanupInterval)
		slog.Info("Cache enabled", "expiration", cfg.Cache.DefaultExpiration)
	}

	// Build routes (this is now done *after* creating dependencies)
	lb.routes = lb.buildRoutes(cfg)
	lb.cfg = cfg // Set the config

	// lb.ReloadConfig(cfg) // We no longer need to call this
	// --- END CHANGE ---

	slog.Info("Load balancer initialized")
	return lb
}

// ... (buildRoutes - no changes) ...
func (lb *LoadBalancer) buildRoutes(cfg *Config) []*Route {
	routes := make([]*Route, 0, len(cfg.Routes))
	for _, routeCfg := range cfg.Routes {
		pool := NewBackendPool(
			routeCfg,
			lb.consul, // <-- PASS THE CONSUL CLIENT
			cfg.CircuitBreaker,
			cfg.ConnectionPool,
		)
		pool.StartHealthChecks()
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// --- Start Logger Retrieval ---
			// 1. Get the logger from the context
			logger, ok := r.Context().Value(loggerKey).(*slog.Logger)
			if !ok {
				logger = slog.Default() // Fallback just in case
			}

			// 2. Add route-specific context!
			logger = logger.With(
				"route_path", routeCfg.Path,
				"route_host", routeCfg.Host,
			)
			// --- End Logger Retrieval ---

			lb.lock.RLock()
			cacheEnabled := lb.cache != nil
			lb.lock.RUnlock()
			var cacheKey string

			if cacheEnabled && r.Method == http.MethodGet {
				cacheKey = r.Host + r.URL.Path
				if item, found := lb.cache.Get(cacheKey); found {
					// Use our new logger
					logger.Debug("Cache hit", "key", cacheKey)
					cachedItem := item.(CacheItem)
					for key, values := range cachedItem.Header {
						for _, value := range values {
							w.Header().Add(key, value)
						}
					}
					w.WriteHeader(cachedItem.Status)
					if _, err := w.Write(cachedItem.Body); err != nil {
						logger.Warn("Error writing cached response", "error", err) // Use logger
					}
					return
				}
				logger.Debug("Cache miss", "key", cacheKey) // Use logger
			}
			backend := pool.GetNextBackend(r)
			if backend == nil {
				// Use logger. All context (req_id, client_ip, route) is added automatically.
				logger.Warn("No healthy backend found for request")
				http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
				return
			}

			// Add backend context for all subsequent logs
			logger = logger.With("backend_addr", backend.Addr)

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
					logger.Info("Proxying request", "host", r.Host, "path", r.URL.Path, "backend", backend.Addr, "circuit_state", backend.cb.State().String())
					backend.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
						logger.Warn("Backend connection error", "backend", backend.Addr, "error", err)
					}
					backend.proxy.ServeHTTP(recorder, r)
					if recorder.Status >= http.StatusInternalServerError {
						return nil, fmt.Errorf("backend %s returned status %d", backend.Addr, recorder.Status)
					}
					return nil, nil
				})
			} else {
				logger.Info("Proxying request", "host", r.Host, "path", r.URL.Path, "backend", backend.Addr)
				backend.proxy.ServeHTTP(recorder, r)
			}
			if requestErr != nil {
				if errors.Is(requestErr, gobreaker.ErrOpenState) || errors.Is(requestErr, gobreaker.ErrTooManyRequests) {
					logger.Warn("Request blocked by circuit breaker", "backend", backend.Addr, "state", backend.cb.State().String())
					http.Error(w, "Service unavailable (Circuit OPEN)", http.StatusServiceUnavailable)
				} else {
					logger.Warn("Circuit breaker failure recorded", "backend", backend.Addr, "error", requestErr)
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
				logger.Debug("Caching response", "key", cacheKey, "size_bytes", len(item.Body))
				lb.cache.Set(cacheKey, item, cache.DefaultExpiration)
			}
			if !recorder.wroteHeader {
				recorder.ResponseWriter.WriteHeader(recorder.Status)
			}
			if _, err := recorder.ResponseWriter.Write(recorder.Body.Bytes()); err != nil {
				logger.Warn("Error writing final response", "error", err)
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
func (lb *LoadBalancer) getRoutes() []*Route {
	lb.lock.RLock()
	defer lb.lock.RUnlock()
	return lb.routes
}
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// --- Start Contextual Logger ---
	// 1. Generate a unique Request ID
	reqIDBytes := make([]byte, 6)
	if _, err := rand.Read(reqIDBytes); err != nil {
		slog.Warn("Failed to generate request ID", "error", err) // Use global logger for this one error
	}
	reqID := hex.EncodeToString(reqIDBytes)

	// 2. Get Client IP
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

	// 3. Create a new logger with request-specific attributes
	logger := slog.Default().With(
		"req_id", reqID,
		"client_ip", clientIP,
	)

	// 4. Store the logger in the request's context
	r = r.WithContext(context.WithValue(r.Context(), loggerKey, logger))
	// --- End Contextual Logger ---

	// Add the Request ID to the response headers so the client can see it
	w.Header().Set("X-Request-ID", reqID)

	lb.lock.RLock()
	limiterEnabled := lb.rl != nil
	lb.lock.RUnlock()

	if limiterEnabled {
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

		// --- THIS IS THE CHANGE ---
		// We no longer get a "limiter" object, we just ask to "Allow"
		if !lb.rl.Allow(clientIP) {
			logger.Warn("Global rate limit exceeded")
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
	logger.Warn("No route found for request", "host", r.Host, "path", r.URL.Path)
	http.NotFound(w, r)
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
