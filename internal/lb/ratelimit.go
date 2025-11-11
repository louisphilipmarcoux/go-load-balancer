package lb

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimiter now holds a Redis client
type RateLimiter struct {
	client *redis.Client
	limit  int // Requests per second
}

// NewRateLimiter connects to Redis
// CHANGED: The signature now takes the specific configs it needs
func NewRateLimiter(rlCfg *RateLimitConfig, redisCfg *RedisConfig) *RateLimiter {
	if redisCfg == nil {
		slog.Error("Redis is not configured, but rate limiting is enabled.")
		return nil
	}
	if rlCfg == nil || rlCfg.RequestsPerSecond == 0 {
		slog.Error("RateLimit config is missing or RequestsPerSecond is 0.")
		return nil
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisCfg.Addr,
		Password: redisCfg.Password,
		DB:       redisCfg.DB,
	})

	// Test the connection
	if _, err := rdb.Ping(context.Background()).Result(); err != nil {
		slog.Error("Failed to connect to Redis for rate limiting", "error", err)
		return nil
	}

	slog.Info("Connected to Redis for global rate limiting")
	return &RateLimiter{
		client: rdb,
		limit:  int(rlCfg.RequestsPerSecond), // Use rlCfg
	}
}

// Allow checks if a request from a given IP is allowed
// This implements a "Fixed Window" algorithm
func (rl *RateLimiter) Allow(ip string) bool {
	ctx := context.Background()
	key := "ratelimit:" + ip

	// Use a pipeline to make the INCR and EXPIRE commands atomic
	pipe := rl.client.Pipeline()
	count := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 1*time.Second) // Set expiration for this 1-second window
	_, err := pipe.Exec(ctx)
	if err != nil {
		slog.Warn("Failed to execute Redis rate limit pipeline", "error", err)
		// Fail open (allow request) if Redis fails
		return true
	}

	// Check if the count exceeds the limit
	if count.Val() > int64(rl.limit) {
		return false // Deny request
	}

	return true // Allow request
}
