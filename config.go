package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ... (Config, TLSConfig, RateLimitConfig, CircuitBreakerConfig, ConnectionPoolConfig, CacheConfig - no changes) ...
type Config struct {
	ListenAddr     string                `yaml:"listenAddr"`
	MetricsAddr    string                `yaml:"metricsAddr"`
	AdminAddr      string                `yaml:"adminAddr"`
	TLS            *TLSConfig            `yaml:"tls"`
	Autocert       *AutocertConfig       `yaml:"autocert"`
	RateLimit      *RateLimitConfig      `yaml:"rateLimit"`
	CircuitBreaker *CircuitBreakerConfig `yaml:"circuitBreaker"`
	ConnectionPool *ConnectionPoolConfig `yaml:"connectionPool"`
	Cache          *CacheConfig          `yaml:"cache"`
	Routes         []*RouteConfig        `yaml:"routes"`
}

// ADD THIS STRUCT BACK
type TLSConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}

// NEW: AutocertConfig holds settings for Let's Encrypt
type AutocertConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Email    string   `yaml:"email"`
	Domains  []string `yaml:"domains"`
	CacheDir string   `yaml:"cacheDir"`
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
type ConnectionPoolConfig struct {
	MaxIdleConns        int           `yaml:"maxIdleConns"`
	MaxIdleConnsPerHost int           `yaml:"maxIdleConnsPerHost"`
	IdleConnTimeout     time.Duration `yaml:"idleConnTimeout"`
}
type CacheConfig struct {
	Enabled           bool          `yaml:"enabled"`
	DefaultExpiration time.Duration `yaml:"defaultExpiration"`
	CleanupInterval   time.Duration `yaml:"cleanupInterval"`
}

// --- THIS BLOCK IS CHANGED ---

// RouteConfig defines a single routing rule
type RouteConfig struct {
	Host     string            `yaml:"host"     json:"host,omitempty"`
	Path     string            `yaml:"path"     json:"path"`
	Headers  map[string]string `yaml:"headers"  json:"headers,omitempty"`
	Strategy string            `yaml:"strategy" json:"strategy"`
	Backends []*BackendConfig  `yaml:"backends" json:"backends"`
}

// BackendConfig defines a single backend server
type BackendConfig struct {
	Addr   string `yaml:"addr"   json:"addr"`
	Weight int    `yaml:"weight" json:"weight"`
}

// --- END OF CHANGE ---

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
