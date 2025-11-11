package lb

import (
	"fmt"
	"log" // We still need 'log' for the fatal error in LoadConfig
	"os"
	"time"

	"github.com/caarlos0/env/v9"
	"gopkg.in/yaml.v3"
)

// ListenerConfig holds settings for a single entrypoint
type ListenerConfig struct {
	Name       string           `yaml:"name"`     // e.g., "web-https", "postgres-tcp"
	Protocol   string           `yaml:"protocol"` // "http", "https", "tcp", "udp"
	ListenAddr string           `yaml:"listenAddr"`
	TLS        *TLSConfig       `yaml:"tls,omitempty"`
	Autocert   *AutocertConfig  `yaml:"autocert,omitempty"`
	RateLimit  *RateLimitConfig `yaml:"rateLimit,omitempty"` // L7
	Cache      *CacheConfig     `yaml:"cache,omitempty"`     // L7
	Routes     []*RouteConfig   `yaml:"routes,omitempty"`    // L7
	Strategy   string           `yaml:"strategy,omitempty"`  // L4
	Backends   []*BackendConfig `yaml:"backends,omitempty"`  // L4
	Service    string           `yaml:"service,omitempty"`   // L4
}

// Config is the top-level configuration
type Config struct {
	// NEW: Listeners holds all listener configurations
	Listeners []*ListenerConfig `yaml:"listeners"`

	// GLOBAL SETTINGS (Shared by all listeners/services)
	MetricsAddr    string                `yaml:"metricsAddr"    env:"METRICS_ADDR"`
	AdminAddr      string                `yaml:"adminAddr"      env:"ADMIN_ADDR"`
	AdminToken     string                `yaml:"adminToken"     env:"ADMIN_TOKEN"`
	CircuitBreaker *CircuitBreakerConfig `yaml:"circuitBreaker"`
	ConnectionPool *ConnectionPoolConfig `yaml:"connectionPool"`
	Redis          *RedisConfig          `yaml:"redis"`
	Consul         *ConsulConfig         `yaml:"consul"`
}

// --- Other structs are mostly unchanged ---

type ConsulConfig struct {
	Addr string `env:"CONSUL_ADDR"`
}

type RedisConfig struct {
	Addr     string `env:"REDIS_ADDR"`
	Password string `env:"REDIS_PASSWORD"`
	DB       int    `env:"REDIS_DB"`
}

type TLSConfig struct {
	CertFile     string   `yaml:"certFile" env:"TLS_CERT_FILE"`
	KeyFile      string   `yaml:"keyFile"  env:"TLS_KEY_FILE"`
	MinVersion   string   `yaml:"minVersion"   env:"TLS_MIN_VERSION"`
	CipherSuites []string `yaml:"cipherSuites" env:"TLS_CIPHER_SUITES" envSeparator:","`
}

type AutocertConfig struct {
	Enabled  bool     `yaml:"enabled"  env:"AUTOCERT_ENABLED"`
	Email    string   `yaml:"email"    env:"AUTOCERT_EMAIL"`
	Domains  []string `yaml:"domains"  env:"AUTOCERT_DOMAINS" envSeparator:","`
	CacheDir string   `yaml:"cacheDir" env:"AUTOCERT_CACHE_DIR"`
}

type RateLimitConfig struct {
	Enabled           bool    `yaml:"enabled"           env:"RATE_LIMIT_ENABLED"`
	RequestsPerSecond float64 `yaml:"requestsPerSecond" env:"RATE_LIMIT_RPS"`
	Burst             int     `yaml:"burst"             env:"RATE_LIMIT_BURST"`
}
type CircuitBreakerConfig struct {
	Enabled             bool          `yaml:"enabled"              env:"CB_ENABLED"`
	ConsecutiveFailures uint32        `yaml:"consecutiveFailures"  env:"CB_CONSECUTIVE_FAILURES"`
	OpenStateTimeout    time.Duration `yaml:"openStateTimeout"     env:"CB_TIMEOUT"`
}
type ConnectionPoolConfig struct {
	MaxIdleConns        int           `yaml:"maxIdleConns"        env:"POOL_MAX_IDLE_CONNS"`
	MaxIdleConnsPerHost int           `yaml:"maxIdleConnsPerHost" env:"POOL_MAX_IDLE_PER_HOST"`
	IdleConnTimeout     time.Duration `yaml:"idleConnTimeout"     env:"POOL_IDLE_TIMEOUT"`
}
type CacheConfig struct {
	Enabled           bool          `yaml:"enabled"            env:"CACHE_ENABLED"`
	DefaultExpiration time.Duration `yaml:"defaultExpiration"  env:"CACHE_EXPIRATION"`
	CleanupInterval   time.Duration `yaml:"cleanupInterval"    env:"CACHE_CLEANUP_INTERVAL"`
}

type RouteConfig struct {
	Host     string            `yaml:"host"     json:"host,omitempty"`
	Path     string            `yaml:"path"     json:"path"`
	Headers  map[string]string `yaml:"headers"  json:"headers,omitempty"`
	Strategy string            `yaml:"strategy" json:"strategy"`
	Backends []*BackendConfig  `yaml:"backends" json:"backends"`
	Service  string            `yaml:"service"  json:"service,omitempty"`
}
type BackendConfig struct {
	Addr   string `yaml:"addr"   json:"addr"`
	Weight int    `yaml:"weight" json:"weight"`
}

// LoadConfig function is unchanged
func LoadConfig(path string) (*Config, error) {
	var cfg Config

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("Warning: failed to close config file: %v", err)
		}
	}()
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config YAML: %w", err)
	}

	if cfg.Redis == nil {
		cfg.Redis = &RedisConfig{}
	}
	if cfg.Consul == nil {
		cfg.Consul = &ConsulConfig{}
	}

	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse env vars: %w", err)
	}

	return &cfg, nil
}
