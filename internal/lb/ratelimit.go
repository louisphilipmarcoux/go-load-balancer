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
		limit:  int(rlCfg.RequestsPerSecond),
	}
}

// Allow checks if a request from a given IP is allowed
func (rl *RateLimiter) Allow(ip string) bool {
	ctx := context.Background()
	key := "ratelimit:" + ip

	pipe := rl.client.Pipeline()
	count := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 1*time.Second)
	_, err := pipe.Exec(ctx)
	if err != nil {
		slog.Warn("Failed to execute Redis rate limit pipeline", "error", err)
		return true // Fail open
	}

	if count.Val() > int64(rl.limit) {
		return false // Deny request
	}

	return true // Allow request
}
