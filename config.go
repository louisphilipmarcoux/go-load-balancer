package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration
type Config struct {
	ListenAddr     string                `yaml:"listenAddr"`
	MetricsAddr    string                `yaml:"metricsAddr"`
	TLS            *TLSConfig            `yaml:"tls"`
	RateLimit      *RateLimitConfig      `yaml:"rateLimit"`
	CircuitBreaker *CircuitBreakerConfig `yaml:"circuitBreaker"`
	ConnectionPool *ConnectionPoolConfig `yaml:"connectionPool"` // NEW
	Routes         []*RouteConfig        `yaml:"routes"`
}

// ... (TLSConfig, RateLimitConfig, CircuitBreakerConfig - no changes) ...
type TLSConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}
type RateLimitConfig struct {
	Enabled           bool    `yaml:"enabled"`
	RequestsPerSecond float64 `yaml:"requestsPerSecond"`
	Burst             int     `yaml:"burst"`
}
type CircuitBreakerConfig struct {
	Enabled             bool          `yaml:"enabled"`
	ConsecutiveFailures uint32        `yaml:"consecutiveFailures"`
	OpenStateTimeout    time.Duration `yaml:"openStateTimeout"`
}

// NEW: ConnectionPoolConfig holds keep-alive settings
type ConnectionPoolConfig struct {
	MaxIdleConns        int           `yaml:"maxIdleConns"`
	MaxIdleConnsPerHost int           `yaml:"maxIdleConnsPerHost"`
	IdleConnTimeout     time.Duration `yaml:"idleConnTimeout"`
}

// ... (RouteConfig, BackendConfig, LoadConfig - no changes) ...
type RouteConfig struct {
	Host     string            `yaml:"host"`
	Path     string            `yaml:"path"`
	Headers  map[string]string `yaml:"headers"`
	Strategy string            `yaml:"strategy"`
	Backends []*BackendConfig  `yaml:"backends"`
}
type BackendConfig struct {
	Addr   string `yaml:"addr"`
	Weight int    `yaml:"weight"`
}

func LoadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("Warning: failed to close config file: %v", err)
		}
	}()
	var cfg Config
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config YAML: %w", err)
	}
	return &cfg, nil
}
