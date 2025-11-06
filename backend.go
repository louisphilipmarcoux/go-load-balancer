package main

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker"
)

// Backend holds the state for a single backend server
type Backend struct {
	Addr          string
	healthy       bool
	lock          sync.RWMutex
	Weight        int
	CurrentWeight int
	connections   uint64
	cb            *gobreaker.CircuitBreaker
}

// IsHealthy checks if the backend is healthy
func (b *Backend) IsHealthy() bool {
	b.lock.RLock()
	defer b.lock.RUnlock()
	return b.healthy
}

// SetHealth updates the health status of the backend
func (b *Backend) SetHealth(healthy bool) {
	b.lock.Lock()
	defer b.lock.Unlock()
	b.healthy = healthy
}

// IncrementConnections increases the connection count
func (b *Backend) IncrementConnections() {
	atomic.AddUint64(&b.connections, 1)
}

// DecrementConnections decreases the connection count
func (b *Backend) DecrementConnections() {
	atomic.AddUint64(&b.connections, ^uint64(0))
}

// GetConnections returns the current connection count
func (b *Backend) GetConnections() uint64 {
	return atomic.LoadUint64(&b.connections)
}

// BackendPool holds a collection of backends for a single route
type BackendPool struct {
	backends []*Backend
	strategy string
	current  uint64
	lock     sync.Mutex
}

// NewBackendPool creates a new backend pool
func NewBackendPool(strategy string, backendConfigs []*BackendConfig, cbCfg *CircuitBreakerConfig) *BackendPool {
	backends := make([]*Backend, 0, len(backendConfigs))
	for _, bc := range backendConfigs {
		weight := 1
		if bc.Weight > 0 {
			weight = bc.Weight
		}

		var cb *gobreaker.CircuitBreaker
		if cbCfg != nil && cbCfg.Enabled {
			st := gobreaker.Settings{
				Name: bc.Addr,
				ReadyToTrip: func(counts gobreaker.Counts) bool {
					return counts.ConsecutiveFailures > cbCfg.ConsecutiveFailures
				},
				Timeout: cbCfg.OpenStateTimeout,
				OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
					log.Printf("CircuitBreaker '%s' state changed: %s -> %s", name, from, to)
				},
			}
			cb = gobreaker.NewCircuitBreaker(st)
		}

		backends = append(backends, &Backend{
			Addr:   bc.Addr,
			Weight: weight,
			cb:     cb,
		})
	}
	return &BackendPool{
		backends: backends,
		strategy: strategy,
	}
}

// healthCheck performs a single health check on a backend
func (p *BackendPool) healthCheck(b *Backend) {
	conn, err := net.DialTimeout("tcp", b.Addr, 2*time.Second)
	if err != nil {
		if b.IsHealthy() {
			log.Printf("Backend %s is DOWN", b.Addr)
			b.SetHealth(false)
		}
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("Warning: failed to close health check connection: %v", err)
		}
	}()
	if !b.IsHealthy() {
		log.Printf("Backend %s is UP", b.Addr)
		b.SetHealth(true)
	}
}

// StartHealthChecks starts the health check ticker
func (p *BackendPool) StartHealthChecks() {
	log.Printf("Starting health checks for %d backends...", len(p.backends))
	for _, b := range p.backends {
		go p.healthCheck(b)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			log.Println("Running scheduled health checks...")
			for _, b := range p.backends {
				if b.cb != nil && b.cb.State() == gobreaker.StateOpen {
					log.Printf("Skipping health check for %s (circuit OPEN)", b.Addr)
					continue
				}
				go p.healthCheck(b)
			}
		}
	}()
}

// GetTotalConnections returns the sum of all backend connections
func (p *BackendPool) GetTotalConnections() uint64 {
	var total uint64
	for _, b := range p.backends {
		total += b.GetConnections()
	}
	return total
}
