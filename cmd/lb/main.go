package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/louisphilipmarcoux/go-load-balancer/internal/lb"
	"golang.org/x/crypto/acme/autocert"
)

// --- Helper functions (getTLSVersion, getCipherSuites, startAutocertChallengeServer) ---
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
		return tls.VersionTLS12
	}
}
func getCipherSuites(names []string) []uint16 {
	if len(names) == 0 {
		return nil
	}
	suiteMap := make(map[string]uint16)
	for _, suite := range tls.CipherSuites() {
		suiteMap[suite.Name] = suite.ID
	}
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

// startHttpListener
func startHttpListener(listenerCfg *lb.ListenerConfig, loadBalancer *lb.LoadBalancer) (mainServer *http.Server, autocertServer *http.Server) {
	var tlsConfig *tls.Config
	if listenerCfg.Autocert != nil && listenerCfg.Autocert.Enabled {
		slog.Info("Autocert is enabled", "name", listenerCfg.Name)
		certManager := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(listenerCfg.Autocert.Domains...),
			Email:      listenerCfg.Autocert.Email,
			Cache:      autocert.DirCache(listenerCfg.Autocert.CacheDir),
		}
		tlsConfig = certManager.TLSConfig()
		autocertServer = startAutocertChallengeServer(certManager)
	} else if listenerCfg.TLS != nil {
		slog.Info("Using static TLS configuration", "name", listenerCfg.Name)
		tlsConfig = &tls.Config{
			MinVersion:   getTLSVersion(listenerCfg.TLS.MinVersion),
			CipherSuites: getCipherSuites(listenerCfg.TLS.CipherSuites),
		}
	} else {
		slog.Info("Listener has no TLS configured", "name", listenerCfg.Name)
	}
	server := &http.Server{
		Addr:      listenerCfg.ListenAddr,
		Handler:   loadBalancer,
		TLSConfig: tlsConfig,
	}
	go func() {
		var err error
		if listenerCfg.Protocol == "https" && tlsConfig != nil {
			slog.Info("HTTPS listener starting", "name", listenerCfg.Name, "addr", server.Addr)
			if listenerCfg.Autocert != nil && listenerCfg.Autocert.Enabled {
				err = server.ListenAndServeTLS("", "")
			} else {
				err = server.ListenAndServeTLS(listenerCfg.TLS.CertFile, listenerCfg.TLS.KeyFile)
			}
		} else {
			slog.Info("HTTP listener starting", "name", listenerCfg.Name, "addr", server.Addr)
			err = server.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Listener failed", "name", listenerCfg.Name, "protocol", listenerCfg.Protocol, "error", err)
			os.Exit(1)
		}
	}()
	return server, autocertServer
}

func main() {
	// --- Logger and Config setup ---
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		logLevel.Set(slog.LevelDebug)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)
	configPath := flag.String("config", "config.yaml", "Path to the configuration file")
	flag.Parse()
	cfg, err := lb.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	loadBalancer := lb.NewLoadBalancer(cfg)

	// --- Global Services ---
	if cfg.MetricsAddr != "" {
		go lb.StartMetricsServer(cfg.MetricsAddr, loadBalancer)
	}
	var adminServer *http.Server
	if cfg.AdminAddr != "" {
		adminServer = lb.StartAdminServer(loadBalancer)
	}

	// --- Listener Loop ---
	var httpServers []*http.Server
	var tcpProxies []*lb.TcpProxy
	var udpProxies []*lb.UDPProxy

	for _, listenerCfg := range cfg.Listeners {
		listenerCfg := listenerCfg // Capture loop variable

		switch listenerCfg.Protocol {
		case "http", "https":
			mainServer, acmeServer := startHttpListener(listenerCfg, loadBalancer)
			httpServers = append(httpServers, mainServer)
			if acmeServer != nil {
				httpServers = append(httpServers, acmeServer)
			}

		case "tcp":
			proxy, err := lb.NewTcpProxy(listenerCfg, loadBalancer)
			if err != nil {
				slog.Error("Failed to create TCP proxy", "name", listenerCfg.Name, "error", err)
				continue
			}
			tcpProxies = append(tcpProxies, proxy)
			go proxy.Run()

		case "udp":
			proxy, err := lb.NewUDPProxy(listenerCfg, loadBalancer)
			if err != nil {
				slog.Error("Failed to create UDP proxy", "name", listenerCfg.Name, "error", err)
				continue
			}
			udpProxies = append(udpProxies, proxy)
			go proxy.Run()

		default:
			slog.Error("Unknown listener protocol, skipping", "name", listenerCfg.Name, "protocol", listenerCfg.Protocol)
		}
	}

	// --- Signal Handling ---
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for sig := range sigChan {
		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			slog.Info("Shutting down... Stopping new connections.")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var wg sync.WaitGroup

			// --- Shutdown UDP Proxies ---
			for _, proxy := range udpProxies {
				proxy.Shutdown()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, proxy := range udpProxies {
					proxy.Wait()
				}
				slog.Info("All UDP listeners shut down.")
			}()

			// --- Shutdown TCP Proxies ---
			for _, proxy := range tcpProxies {
				proxy.Shutdown()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, proxy := range tcpProxies {
					proxy.Wait()
				}
				slog.Info("All TCP listeners shut down.")
			}()

			// --- Shutdown HTTP Servers ---
			for _, srv := range httpServers {
				wg.Add(1)
				srv := srv
				go func() {
					defer wg.Done()
					if err := srv.Shutdown(ctx); err != nil {
						slog.Warn("HTTP server shutdown failed", "error", err)
					} else {
						slog.Info("HTTP server shutdown complete.")
					}
				}()
			}

			// Shutdown admin server
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
			}
		}
	}
}
