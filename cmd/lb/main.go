package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"      // Keep for the one-time Fatalf
	"log/slog" // NEW
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/louisphilipmarcoux/go-load-balancer/internal/lb"
	"golang.org/x/crypto/acme/autocert"
)

// getTLSVersion maps a string config value to a Go tls.Version constant
func getTLSVersion(version string) uint16 {
	switch version {
	case "tls1.0":
		return tls.VersionTLS10
	case "tls1.1":
		return tls.VersionTLS11
	case "tls1.2":
		return tls.VersionTLS12
	case "tls1.3":
		return tls.VersionTLS13
	default:
		// Default to a strong version if config is missing or invalid
		return tls.VersionTLS12
	}
}

// getCipherSuites maps a list of string names to Go tls.CipherSuite constants
func getCipherSuites(names []string) []uint16 {
	if len(names) == 0 {
		return nil // Use Go's default cipher suites
	}

	// This map is built from Go's standard tls.CipherSuites() list
	suiteMap := make(map[string]uint16)
	for _, suite := range tls.CipherSuites() {
		suiteMap[suite.Name] = suite.ID
	}
	// Add Go's non-standard (but still valid) names
	for _, suite := range tls.InsecureCipherSuites() {
		suiteMap[suite.Name] = suite.ID
	}

	var suites []uint16
	for _, name := range names {
		if id, ok := suiteMap[name]; ok {
			suites = append(suites, id)
		} else {
			slog.Warn("Unknown TLS cipher suite specified in config, skipping", "cipher", name)
		}
	}
	return suites
}

func startAutocertChallengeServer(certManager *autocert.Manager) *http.Server {
	server := &http.Server{
		Addr:    ":80",
		Handler: certManager.HTTPHandler(nil),
	}
	go func() {
		slog.Info("Starting Autocert HTTP-01 challenge server", "addr", ":80")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Autocert server failed", "error", err)
			slog.Warn("Automatic certificate management will fail")
		}
	}()
	return server
}

func main() {
	// --- NEW: Structured Logger Setup ---
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo) // Default
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		logLevel.Set(slog.LevelDebug)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)
	// --- End Logger Setup ---

	configPath := flag.String("config", "config.yaml", "Path to the configuration file")
	flag.Parse()
	cfg, err := lb.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Renamed from 'lb' to 'loadBalancer' to avoid shadowing the package name
	loadBalancer := lb.NewLoadBalancer(cfg)

	var tlsConfig *tls.Config
	var autocertServer *http.Server

	if cfg.Autocert != nil && cfg.Autocert.Enabled {
		slog.Info("Autocert is enabled")
		certManager := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Autocert.Domains...),
			Email:      cfg.Autocert.Email,
			Cache:      autocert.DirCache(cfg.Autocert.CacheDir),
		}
		tlsConfig = certManager.TLSConfig()
		autocertServer = startAutocertChallengeServer(certManager)

	} else if cfg.TLS != nil {
		slog.Info("Using static TLS configuration")
		tlsConfig = &tls.Config{
			MinVersion:   getTLSVersion(cfg.TLS.MinVersion),
			CipherSuites: getCipherSuites(cfg.TLS.CipherSuites),
		}
	} else {
		slog.Info("TLS is not configured")
	}

	server := &http.Server{
		Addr:      cfg.ListenAddr,
		Handler:   loadBalancer,
		TLSConfig: tlsConfig,
	}

	go func() {
		if cfg.Autocert != nil && cfg.Autocert.Enabled {
			slog.Info("Load Balancer (L7/Autocert) listening", "addr", server.Addr)
			if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("Autocert load balancer failed", "error", err)
				os.Exit(1)
			}
		} else if cfg.TLS != nil {
			slog.Info("Load Balancer (L7/Static TLS) listening", "addr", server.Addr)
			if err := server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("Static TLS load balancer failed", "error", err)
				os.Exit(1)
			}
		} else {
			slog.Info("Load Balancer (L7/HTTP) listening", "addr", server.Addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("HTTP load balancer failed", "error", err)
				os.Exit(1)
			}
		}
	}()

	if cfg.MetricsAddr != "" {
		go lb.StartMetricsServer(cfg.MetricsAddr, loadBalancer)
	}

	var adminServer *http.Server
	if cfg.AdminAddr != "" {
		adminServer = lb.StartAdminServer(loadBalancer)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for sig := range sigChan {
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			slog.Info("Shutting down... Stopping new connections.")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var wg sync.WaitGroup

			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := server.Shutdown(ctx); err != nil {
					slog.Warn("Main server shutdown failed", "error", err)
				} else {
					slog.Info("Main server shutdown complete.")
				}
			}()

			if adminServer != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := adminServer.Shutdown(ctx); err != nil {
						slog.Warn("Admin server shutdown failed", "error", err)
					} else {
						slog.Info("Admin server shutdown complete.")
					}
				}()
			}

			if autocertServer != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := autocertServer.Shutdown(ctx); err != nil {
						slog.Warn("Autocert server shutdown failed", "error", err)
					} else {
						slog.Info("Autocert server shutdown complete.")
					}
				}()
			}

			wg.Wait()
			slog.Info("Graceful shutdown complete.")
			return

		case syscall.SIGHUP:
			slog.Info("SIGHUP received, reloading configuration...")
			newCfg, err := lb.LoadConfig(*configPath)
			if err != nil {
				slog.Error("Failed to reload config", "error", err)
			} else {
				loadBalancer.ReloadConfig(newCfg)
				slog.Info("Configuration successfully reloaded")
			}
		}
	}
}
