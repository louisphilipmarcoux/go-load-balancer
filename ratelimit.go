package main

import (
	"sync"

	"golang.org/x/time/rate"
)

// RateLimiter holds the limiters for all client IPs
type RateLimiter struct {
	clients map[string]*rate.Limiter
	lock    sync.Mutex
	rlCfg   *RateLimitConfig
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(rlCfg *RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		clients: make(map[string]*rate.Limiter),
		rlCfg:   rlCfg,
	}
}

// GetLimiter gets or creates a limiter for a given IP
func (rl *RateLimiter) GetLimiter(ip string) *rate.Limiter {
	rl.lock.Lock()
	defer rl.lock.Unlock()

	limiter, exists := rl.clients[ip]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(rl.rlCfg.RequestsPerSecond), rl.rlCfg.Burst)
		rl.clients[ip] = limiter
	}
	return limiter
}
