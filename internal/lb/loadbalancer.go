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
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/patrickmn/go-cache"
	"github.com/sony/gobreaker"
)

// contextKey and Route struct (Unchanged)
type contextKey string

const loggerKey = contextKey("logger")

type Route struct {
	config  *RouteConfig
	pool    *BackendPool
	handler http.Handler
}

// Matches (Unchanged)
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

// LoadBalancer struct
type LoadBalancer struct {
	cfg     *Config
	routes  []*Route                // L7 routes
	l4Pools map[string]*BackendPool // L4 pools, mapped by listener name
	rl      *RateLimiter
	cache   *cache.Cache
	lock    sync.RWMutex
}

// CacheItem (Unchanged)
type CacheItem struct {
	Status int
	Header http.Header
	Body   []byte
}

// reloadConfig_unsafe (CHANGED)
func (lb *LoadBalancer) reloadConfig_unsafe(cfg *Config) {
	// Consul logic is removed
	newRoutes := lb.buildL7Routes(cfg)
	newL4Pools := lb.buildL4Pools(cfg)

	lb.cfg = cfg
	lb.routes = newRoutes
	lb.l4Pools = newL4Pools
	slog.Info("Configuration successfully reloaded (unsafe)")
}

// ReloadConfig (Unchanged)
func (lb *LoadBalancer) ReloadConfig(cfg *Config) {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	lb.reloadConfig_unsafe(cfg)
	slog.Info("Configuration successfully reloaded (safe)")
}

// NewLoadBalancer (CHANGED)
func NewLoadBalancer(cfg *Config) *LoadBalancer {
	lb := &LoadBalancer{}

	// Consul logic is removed
	for _, l := range cfg.Listeners {
		if l.Protocol == "http" || l.Protocol == "https" {
			if l.RateLimit != nil && l.RateLimit.Enabled {
				lb.rl = NewRateLimiter(l.RateLimit, cfg.Redis)
				if lb.rl == nil {
					slog.Warn("Rate limiter initialization failed, disabling.")
				}
			}
			if l.Cache != nil && l.Cache.Enabled {
				lb.cache = cache.New(l.Cache.DefaultExpiration, l.Cache.CleanupInterval)
				slog.Info("Cache enabled", "expiration", l.Cache.DefaultExpiration)
			}
			break
		}
	}

	lb.routes = lb.buildL7Routes(cfg)
	lb.l4Pools = lb.buildL4Pools(cfg)
	lb.cfg = cfg

	slog.Info("Load balancer initialized")
	return lb
}

// buildL4Pools (CHANGED)
func (lb *LoadBalancer) buildL4Pools(cfg *Config) map[string]*BackendPool {
	pools := make(map[string]*BackendPool)
	for _, listenerCfg := range cfg.Listeners {
		if listenerCfg.Protocol == "tcp" || listenerCfg.Protocol == "udp" {
			slog.Info("Building L4 pool", "listener", listenerCfg.Name, "protocol", listenerCfg.Protocol)
			pools[listenerCfg.Name] = NewL4BackendPool(
				listenerCfg,
				cfg.CircuitBreaker,
			)
		}
	}
	return pools
}

// buildL7Routes (CHANGED)
func (lb *LoadBalancer) buildL7Routes(cfg *Config) []*Route {
	routes := make([]*Route, 0)
	for _, listenerCfg := range cfg.Listeners {
		if listenerCfg.Protocol != "http" && listenerCfg.Protocol != "https" {
			continue
		}

		for _, routeCfg := range listenerCfg.Routes {
			routeCfg := routeCfg // Capture loop variable
			pool := NewL7BackendPool(
				routeCfg,
				cfg.CircuitBreaker,
				cfg.ConnectionPool,
			)

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// ... (handler logic is unchanged) ...
				logger, ok := r.Context().Value(loggerKey).(*slog.Logger)
				if !ok {
					logger = slog.Default()
				}
				logger = logger.With(
					"route_path", routeCfg.Path,
					"route_host", routeCfg.Host,
				)

				lb.lock.RLock()
				cacheEnabled := lb.cache != nil
				lb.lock.RUnlock()
				var cacheKey string
				if cacheEnabled && r.Method == http.MethodGet {
					cacheKey = r.Host + r.URL.Path
					if item, found := lb.cache.Get(cacheKey); found {
						logger.Debug("Cache hit", "key", cacheKey)
						cachedItem := item.(CacheItem)
						for key, values := range cachedItem.Header {
							for _, value := range values {
								w.Header().Add(key, value)
							}
						}
						w.WriteHeader(cachedItem.Status)
						if _, err := w.Write(cachedItem.Body); err != nil {
							logger.Warn("Error writing cached response", "error", err)
						}
						return
					}
					logger.Debug("Cache miss", "key", cacheKey)
				}

				clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
				backend := pool.GetNextBackend(r, clientIP)
				if backend == nil {
					logger.Warn("No healthy backend found for request")
					http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
					return
				}
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
	}

	// Sort routes (unchanged)
	sort.Slice(routes, func(i, j int) bool {
		r1 := routes[i].config
		r2 := routes[j].config
		if r1.Host != "" && r2.Host == "" {
			return true
		}
		if r1.Host == "" && r2.Host != "" {
			return false
		}
		if len(r1.Path) > len(r2.Path) {
			return true
		}
		if len(r1.Path) < len(r2.Path) {
			return false
		}
		return len(r1.Host) > len(r2.Host)
	})

	return routes
}

// ... (ServeHTTP and Admin API helpers are all unchanged) ...
// getRoutes
func (lb *LoadBalancer) getRoutes() []*Route {
	lb.lock.RLock()
	defer lb.lock.RUnlock()
	return lb.routes
}

// ServeHTTP
func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqIDBytes := make([]byte, 6)
	if _, err := rand.Read(reqIDBytes); err != nil {
		slog.Warn("Failed to generate request ID", "error", err)
	}
	reqID := hex.EncodeToString(reqIDBytes)
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	logger := slog.Default().With(
		"req_id", reqID,
		"client_ip", clientIP,
	)
	r = r.WithContext(context.WithValue(r.Context(), loggerKey, logger))
	w.Header().Set("X-Request-ID", reqID)

	lb.lock.RLock()
	limiterEnabled := lb.rl != nil
	lb.lock.RUnlock()

	if limiterEnabled {
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

// --- Admin API Helper Methods (Unchanged) ---
func (lb *LoadBalancer) getRouteByIndex(listener *ListenerConfig, index int) (*RouteConfig, error) {
	if index < 0 || index >= len(listener.Routes) {
		return nil, fmt.Errorf("route index %d out of bounds", index)
	}
	return listener.Routes[index], nil
}
func (lb *LoadBalancer) AddRoute(listener *ListenerConfig, newRoute *RouteConfig) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	for _, route := range listener.Routes {
		if route.Path == newRoute.Path && route.Host == newRoute.Host {
			return fmt.Errorf("route with path '%s' and host '%s' already exists", newRoute.Path, newRoute.Host)
		}
	}
	listener.Routes = append(listener.Routes, newRoute)
	lb.reloadConfig_unsafe(lb.cfg)
	return nil
}
func (lb *LoadBalancer) DeleteRoute(listener *ListenerConfig, index int) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	if _, err := lb.getRouteByIndex(listener, index); err != nil {
		return err
	}
	listener.Routes = append(listener.Routes[:index], listener.Routes[index+1:]...)
	lb.reloadConfig_unsafe(lb.cfg)
	return nil
}
func (lb *LoadBalancer) AddBackendToRoute(listener *ListenerConfig, index int, newBackend *BackendConfig) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	route, err := lb.getRouteByIndex(listener, index)
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
	lb.reloadConfig_unsafe(lb.cfg)
	return nil
}
func (lb *LoadBalancer) DeleteBackendFromRoute(listener *ListenerConfig, index int, host string) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	route, err := lb.getRouteByIndex(listener, index)
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
	lb.reloadConfig_unsafe(lb.cfg)
	return nil
}
func (lb *LoadBalancer) UpdateBackendWeightInRoute(listener *ListenerConfig, index int, host string, weight int) error {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	route, err := lb.getRouteByIndex(listener, index)
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
	lb.reloadConfig_unsafe(lb.cfg)
	return nil
}
