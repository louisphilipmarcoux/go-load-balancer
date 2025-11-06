package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
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

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: lb,
	}

	if cfg.MetricsAddr != "" {
		go StartMetricsServer(cfg.MetricsAddr, lb)
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

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for sig := range sigChan {
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			log.Println("Shutting down... Stopping new connections.")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if err := server.Shutdown(ctx); err != nil {
				log.Printf("Graceful shutdown failed: %v", err)
			} else {
				log.Println("Graceful shutdown complete.")
			}
			return

		case syscall.SIGHUP:
			log.Println("Reloading configuration...")
			newCfg, err := LoadConfig(configPath)
			if err != nil {
				log.Printf("Failed to reload config: %v", err)
			} else {
				lb.ReloadConfig(newCfg)
			}
		}
	}
}
