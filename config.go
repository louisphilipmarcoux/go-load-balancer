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
	Routes         []*RouteConfig        `yaml:"routes"`
}

// TLSConfig holds TLS certificate paths
type TLSConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}

// RateLimitConfig holds rate limiter settings
type RateLimitConfig struct {
	Enabled           bool    `yaml:"enabled"`
	RequestsPerSecond float64 `yaml:"requestsPerSecond"`
	Burst             int     `yaml:"burst"`
}

// CircuitBreakerConfig holds circuit breaker settings
type CircuitBreakerConfig struct {
	Enabled             bool          `yaml:"enabled"`
	ConsecutiveFailures uint32        `yaml:"consecutiveFailures"`
	OpenStateTimeout    time.Duration `yaml:"openStateTimeout"`
}

// RouteConfig defines a single routing rule
type RouteConfig struct {
	Host     string            `yaml:"host"`    // STAGE 19: Match by host header
	Path     string            `yaml:"path"`    // Match by path prefix
	Headers  map[string]string `yaml:"headers"` // STAGE 19: Match by headers
	Strategy string            `yaml:"strategy"`
	Backends []*BackendConfig  `yaml:"backends"`
}

// BackendConfig defines a single backend server
type BackendConfig struct {
	Addr   string `yaml:"addr"`
	Weight int    `yaml:"weight"`
}

// LoadConfig reads and parses the configuration file
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
