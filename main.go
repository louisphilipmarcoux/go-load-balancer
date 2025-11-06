package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// ... (startAutocertChallengeServer - no change) ...
func startAutocertChallengeServer(certManager *autocert.Manager) *http.Server {
	server := &http.Server{
		Addr:    ":80",
		Handler: certManager.HTTPHandler(nil),
	}
	go func() {
		log.Println("Starting Autocert HTTP-01 challenge server on :80")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Autocert server failed: %v", err)
			log.Println("WARNING: Automatic certificate management will fail.")
		}
	}()
	return server
}

func main() {
	configPath := "config.yaml"
	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	lb := NewLoadBalancer(cfg)

	// --- Autocert and TLS Setup ---
	var tlsConfig *tls.Config
	var autocertServer *http.Server

	if cfg.Autocert != nil && cfg.Autocert.Enabled {
		log.Println("Autocert is enabled.")
		certManager := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Autocert.Domains...),
			Email:      cfg.Autocert.Email,
			Cache:      autocert.DirCache(cfg.Autocert.CacheDir),
		}
		tlsConfig = certManager.TLSConfig()
		autocertServer = startAutocertChallengeServer(certManager)

	} else if cfg.TLS != nil {
		log.Println("Using static TLS configuration.")
		// We will use the file paths directly, so tlsConfig remains nil
		// This is the mode your test will use.
	} else {
		log.Println("TLS is not configured.")
	}

	// --- Main LB Server ---
	server := &http.Server{
		Addr:      cfg.ListenAddr,
		Handler:   lb,
		TLSConfig: tlsConfig, // This is nil for static certs, which is correct
	}

	go func() {
		// --- THIS IS THE UPDATED LOGIC ---
		if cfg.Autocert != nil && cfg.Autocert.Enabled {
			// --- Autocert (dynamic certs) ---
			log.Printf("Load Balancer (L7/Autocert) listening on %s", server.Addr)
			// server.TLSConfig is used, so file paths are empty
			if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Failed to run Autocert load balancer: %v", err)
			}
		} else if cfg.TLS != nil {
			// --- Static TLS (from file) ---
			log.Printf("Load Balancer (L7/Static TLS) listening on %s", server.Addr)
			// server.TLSConfig is nil, so we provide file paths
			if err := server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Failed to run static TLS load balancer: %v", err)
			}
		} else {
			// --- Plain HTTP ---
			log.Printf("Load Balancer (L7/HTTP) listening on %s", server.Addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Failed to run HTTP load balancer: %v", err)
			}
		}
	}()

	// --- Metrics Server ---
	if cfg.MetricsAddr != "" {
		go StartMetricsServer(cfg.MetricsAddr, lb)
	}

	// --- Admin Server ---
	var adminServer *http.Server
	if cfg.AdminAddr != "" {
		adminServer = StartAdminServer(lb)
	}

	// --- Signal Handling ---
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for sig := range sigChan {
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			log.Println("Shutting down... Stopping new connections.")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var wg sync.WaitGroup

			// Shutdown main server
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := server.Shutdown(ctx); err != nil {
					log.Printf("Main server shutdown failed: %v", err)
				} else {
					log.Println("Main server shutdown complete.")
				}
			}()

			// Shutdown admin server
			if adminServer != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := adminServer.Shutdown(ctx); err != nil {
						log.Printf("Admin server shutdown failed: %v", err)
					} else {
						log.Println("Admin server shutdown complete.")
					}
				}()
			}

			// Shutdown autocert server
			if autocertServer != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := autocertServer.Shutdown(ctx); err != nil {
						log.Printf("Autocert server shutdown failed: %v", err)
					} else {
						log.Println("Autocert server shutdown complete.")
					}
				}()
			}

			wg.Wait()
			log.Println("Graceful shutdown complete.")
			return

		case syscall.SIGHUP:
			log.Println("Reloading configuration from disk...")
			newCfg, err := LoadConfig(configPath)
			if err != nil {
				log.Printf("Failed to reload config: %v", err)
			} else {
				lb.ReloadConfig(newCfg)
			}
		}
	}
}
