package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync" // NEW
	"syscall"
	"time"
)

func main() {
	configPath := "config.yaml"
	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	lb := NewLoadBalancer(cfg)

	// --- Main LB Server ---
	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: lb,
	}
	go func() {
		if cfg.TLS != nil {
			log.Printf("Load Balancer (L7/TLS) listening on %s", server.Addr)
			if err := server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("Failed to run TLS load balancer: %v", err)
			}
		} else {
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

	// --- Admin Server (NEW) ---
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
			// --- Graceful Shutdown ---
			log.Println("Shutting down... Stopping new connections.")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Use a WaitGroup to shut down servers in parallel
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

			// Shutdown admin server (if it exists)
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

			// Wait for all servers to shut down
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
