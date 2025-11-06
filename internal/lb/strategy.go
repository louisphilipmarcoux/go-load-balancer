package lb

import (
	"hash/fnv"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/sony/gobreaker"
)

// GetNextBackend selects a backend using the pool's strategy
func (p *BackendPool) GetNextBackend(r *http.Request) *Backend {
	switch p.strategy {
	case "ip-hash":
		clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			slog.Warn("Failed to get client IP for ip-hash", "error", err, "fallback", "round-robin")
			return p.GetNextBackendByRoundRobin()
		}
		return p.GetNextBackendByIP(clientIP)
	case "least-connections":
		return p.GetNextBackendByLeastConns()
	case "weighted-least-connections":
		return p.GetNextBackendByWeightedLeastConns()
	case "weighted-round-robin":
		return p.GetNextBackendByWeightedRoundRobin()
	case "round-robin":
		fallthrough
	default:
		return p.GetNextBackendByRoundRobin()
	}
}

// --- Strategy Implementations ---

func (p *BackendPool) GetNextBackendByIP(ip string) *Backend {
	healthyBackends := make([]*Backend, 0)
	for _, backend := range p.backends {
		if backend.IsHealthy() && (backend.cb == nil || backend.cb.State() != gobreaker.StateOpen) {
			healthyBackends = append(healthyBackends, backend)
		}
	}
	if len(healthyBackends) == 0 {
		return nil
	}
	h := fnv.New32a()
	h.Write([]byte(ip))
	index := int(h.Sum32()) % len(healthyBackends)
	return healthyBackends[index]
}

func (p *BackendPool) GetNextBackendByLeastConns() *Backend {
	var bestBackend *Backend
	minConnections := uint64(math.MaxUint64)
	for _, backend := range p.backends {
		if !backend.IsHealthy() || (backend.cb != nil && backend.cb.State() == gobreaker.StateOpen) {
			continue
		}
		conns := backend.GetConnections()
		if conns < minConnections {
			minConnections = conns
			bestBackend = backend
		}
	}
	return bestBackend
}

func (p *BackendPool) GetNextBackendByRoundRobin() *Backend {
	numBackends := uint64(len(p.backends))
	if numBackends == 0 {
		return nil
	}
	for i := uint64(0); i < numBackends; i++ {
		index := atomic.AddUint64(&p.current, 1) % numBackends
		backend := p.backends[index]
		if backend.IsHealthy() && (backend.cb == nil || backend.cb.State() != gobreaker.StateOpen) {
			return backend
		}
	}
	return nil
}

func (p *BackendPool) GetNextBackendByWeightedRoundRobin() *Backend {
	p.lock.Lock()
	defer p.lock.Unlock()
	var bestBackend *Backend
	totalWeight := 0
	for _, backend := range p.backends {
		if !backend.IsHealthy() || (backend.cb != nil && backend.cb.State() == gobreaker.StateOpen) {
			continue
		}
		backend.CurrentWeight += backend.Weight
		totalWeight += backend.Weight
		if bestBackend == nil || backend.CurrentWeight > bestBackend.CurrentWeight {
			bestBackend = backend
		}
	}
	if bestBackend == nil {
		return nil
	}
	bestBackend.CurrentWeight -= totalWeight
	return bestBackend
}

func (p *BackendPool) GetNextBackendByWeightedLeastConns() *Backend {
	var bestBackend *Backend
	minScore := math.Inf(1)
	for _, backend := range p.backends {
		if !backend.IsHealthy() || (backend.cb != nil && backend.cb.State() == gobreaker.StateOpen) {
			continue
		}
		conns := float64(backend.GetConnections())
		weight := float64(backend.Weight)
		score := conns / weight
		if score < minScore {
			minScore = score
			bestBackend = backend
		}
	}
	return bestBackend
}
