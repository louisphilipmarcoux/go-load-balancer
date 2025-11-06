package lb

import (
	"fmt"
	"log" // We still need 'log' for the fatal error in LoadConfig
	"os"
	"time"

	"github.com/caarlos0/env/v9" // NEW
	"gopkg.in/yaml.v3"
)

type RedisConfig struct {
	Addr     string `env:"REDIS_ADDR"`
	Password string `env:"REDIS_PASSWORD"`
	DB       int    `env:"REDIS_DB"`
}

// Config is the top-level configuration
type Config struct {
	ListenAddr     string                `yaml:"listenAddr"     env:"LISTEN_ADDR"`
	MetricsAddr    string                `yaml:"metricsAddr"    env:"METRICS_ADDR"`
	AdminAddr      string                `yaml:"adminAddr"      env:"ADMIN_ADDR"`
	AdminToken     string                `yaml:"adminToken"     env:"ADMIN_TOKEN"` // NEW ENV TAG
	TLS            *TLSConfig            `yaml:"tls"`
	Autocert       *AutocertConfig       `yaml:"autocert"`
	RateLimit      *RateLimitConfig      `yaml:"rateLimit"`
	CircuitBreaker *CircuitBreakerConfig `yaml:"circuitBreaker"`
	ConnectionPool *ConnectionPoolConfig `yaml:"connectionPool"`
	Cache          *CacheConfig          `yaml:"cache"`
	Redis          *RedisConfig          `yaml:"redis"`
	Routes         []*RouteConfig        `yaml:"routes"`
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
	Domains  []string `yaml:"domains"  env:"AUTOCERT_DOMAINS" envSeparator:","` // Comma-separated
	CacheDir string   `yaml:"cacheDir" env:"AUTOCERT_CACHE_DIR"`
}

// ... (Add env tags to all other config structs as well) ...
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

// ... (RouteConfig and BackendConfig are unchanged, as they are lists) ...
type RouteConfig struct {
	Host     string            `yaml:"host"     json:"host,omitempty"`
	Path     string            `yaml:"path"     json:"path"`
	Headers  map[string]string `yaml:"headers"  json:"headers,omitempty"`
	Strategy string            `yaml:"strategy" json:"strategy"`
	Backends []*BackendConfig  `yaml:"backends" json:"backends"`
}
type BackendConfig struct {
	Addr   string `yaml:"addr"   json:"addr"`
	Weight int    `yaml:"weight" json:"weight"`
}

// LoadConfig now reads from YAML *and* overrides with Env Vars
func LoadConfig(path string) (*Config, error) {
	var cfg Config

	// 1. Load from YAML file first
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			// Use standard log here, as slog isn't set up yet
			log.Printf("Warning: failed to close config file: %v", err)
		}
	}()
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config YAML: %w", err)
	}

	// Manually initialize the RedisConfig struct if it's nil.
	// This allows env.Parse to populate it even if it's not in the YAML.
	if cfg.Redis == nil {
		cfg.Redis = &RedisConfig{}
	}

	// 2. NEW: Override with environment variables
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse env vars: %w", err)
	}

	return &cfg, nil
}
