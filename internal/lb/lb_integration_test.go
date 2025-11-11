package lb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/louisphilipmarcoux/go-load-balancer/internal/backend"
)

// --- Constants ---
const (
	lbAddr         = "127.0.0.1:8443"
	lbTcpAddr      = "127.0.0.1:5432"
	lbUdpAddr      = "127.0.0.1:8125"
	metricsAddr    = "127.0.0.1:9090"
	backendAddrAPI = "127.0.0.1:9091"
	backendID_API  = "API-Server-1"
	backendAddrMob = "127.0.0.1:9092"
	backendID_Mob  = "Mobile-Server-1"
	backendAddrWeb = "127.0.0.1:9093"
	backendID_Web  = "Web-Server-1"
	backendAddrUDP = "127.0.0.1:9094"
	backendID_UDP  = "UDP-Server-1"
	backendAddrTCP = "127.0.0.1:9095"
	backendID_TCP  = "TCP-Server-1"
)

var testClient *http.Client

// waitForPort
func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("port %s never became available", addr)
		}
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil && conn != nil {
			if err := conn.Close(); err != nil {
				log.Printf("Warning: failed to close connection during port check: %v", err)
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestMain
func TestMain(m *testing.M) {
	// --- Start HTTP Backends ---
	apiListener, err := backend.RunServer("9091", backendID_API)
	if err != nil {
		log.Fatalf("Failed to start API backend: %v", err)
	}
	defer func() { _ = apiListener.Close() }()
	mobListener, err := backend.RunServer("9092", backendID_Mob)
	if err != nil {
		log.Fatalf("Failed to start Mobile backend: %v", err)
	}
	defer func() { _ = mobListener.Close() }()
	webListener, err := backend.RunServer("9093", backendID_Web)
	if err != nil {
		log.Fatalf("Failed to start Web backend: %v", err)
	}
	defer func() { _ = webListener.Close() }()

	// --- Start UDP Backend ---
	udpListener, err := backend.RunUDPServer("9094", backendID_UDP)
	if err != nil {
		log.Fatalf("Failed to start UDP backend: %v", err)
	}
	defer func() { _ = udpListener.Close() }()

	// --- Start TCP Backend (uses the HTTP server for a simple TCP echo) ---
	tcpListener, err := backend.RunServer("9095", backendID_TCP) // It's just a TCP listener
	if err != nil {
		log.Fatalf("Failed to start TCP backend: %v", err)
	}
	defer func() { _ = tcpListener.Close() }()

	// --- Config ---
	cfg := &Config{
		MetricsAddr: metricsAddr,
		ConnectionPool: &ConnectionPoolConfig{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			IdleConnTimeout:     30 * time.Second,
		},
		Listeners: []*ListenerConfig{
			// L7 HTTPS Listener
			{
				Name:       "integration-test-https",
				Protocol:   "https",
				ListenAddr: lbAddr,
				TLS: &TLSConfig{
					CertFile: "../../server.crt",
					KeyFile:  "../../server.key",
				},
				Routes: []*RouteConfig{
					{
						Host:     "api.example.com",
						Path:     "/",
						Strategy: "round-robin",
						Backends: []*BackendConfig{{Addr: backendAddrAPI, Weight: 1}},
					},
					{
						Path:     "/",
						Headers:  map[string]string{"User-Agent": "MobileApp"},
						Strategy: "round-robin",
						Backends: []*BackendConfig{{Addr: backendAddrMob, Weight: 1}},
					},
					{
						Path:     "/",
						Strategy: "round-robin",
						Backends: []*BackendConfig{{Addr: backendAddrWeb, Weight: 1}},
					},
				},
			},
			// L4 UDP Listener
			{
				Name:       "integration-test-udp",
				Protocol:   "udp",
				ListenAddr: lbUdpAddr,
				Strategy:   "round-robin",
				Backends: []*BackendConfig{
					{Addr: backendAddrUDP, Weight: 1},
				},
			},
			// L4 TCP Listener
			{
				Name:       "integration-test-tcp",
				Protocol:   "tcp",
				ListenAddr: lbTcpAddr,
				Strategy:   "round-robin",
				Backends: []*BackendConfig{
					{Addr: backendAddrTCP, Weight: 1},
				},
			},
		},
	}

	// --- Start Load Balancer ---
	loadBalancer := NewLoadBalancer(cfg)

	var httpServers []*http.Server
	var tcpProxies []*TcpProxy
	var udpProxies []*UDPProxy

	for _, listenerCfg := range cfg.Listeners {
		switch listenerCfg.Protocol {
		case "http", "https":
			var tlsConfig *tls.Config
			if listenerCfg.TLS != nil {
				tlsConfig = &tls.Config{}
			}
			server := &http.Server{
				Addr:      listenerCfg.ListenAddr,
				Handler:   loadBalancer,
				TLSConfig: tlsConfig,
			}
			go func() {
				if err := server.ListenAndServeTLS(listenerCfg.TLS.CertFile, listenerCfg.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("LB test server exited: %v", err)
				}
			}()
			httpServers = append(httpServers, server)

		case "tcp":
			proxy, err := NewTcpProxy(listenerCfg, loadBalancer)
			if err != nil {
				log.Fatalf("Failed to create TCP proxy for test: %v", err)
			}
			tcpProxies = append(tcpProxies, proxy)
			go proxy.Run()
		case "udp":
			proxy, err := NewUDPProxy(listenerCfg, loadBalancer)
			if err != nil {
				log.Fatalf("Failed to create UDP proxy for test: %v", err)
			}
			udpProxies = append(udpProxies, proxy)
			go proxy.Run()
		}
	}

	// Add graceful shutdown for all test servers
	defer func() {
		for _, srv := range httpServers {
			_ = srv.Shutdown(context.Background())
		}
		for _, proxy := range tcpProxies {
			proxy.Shutdown()
			proxy.Wait()
		}
		for _, proxy := range udpProxies {
			proxy.Shutdown()
			proxy.Wait()
		}
	}()

	// Start metrics server
	go StartMetricsServer(cfg.MetricsAddr, loadBalancer)

	// Wait for TCP ports to be ready
	tcpPorts := []string{backendAddrAPI, backendAddrMob, backendAddrWeb, lbAddr, metricsAddr}
	for _, port := range tcpPorts {
		if err := waitForPort(port, 2*time.Second); err != nil {
			log.Fatalf("Server on %s failed to start: %v", port, err)
		}
	}

	testClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	m.Run()
}

// --- TestL4_TCPProxy ---
func TestL4_TCPProxy(t *testing.T) {
	// Dial the load balancer's TCP port
	conn, err := net.Dial("tcp", lbTcpAddr)
	if err != nil {
		t.Fatalf("Failed to dial TCP load balancer: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Warn("Failed to close connection", "error", err)
		}
	}()

	// Send a simple HTTP GET request. We can do this because our
	// test backend (RunServer) is an HTTP server.
	req := "GET / HTTP/1.0\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("Failed to write TCP packet: %v", err)
	}

	// Read the response
	buffer := make([]byte, 1024)
	n, err := conn.Read(buffer)
	if err != nil && err != io.EOF { // EOF is expected
		t.Fatalf("Failed to read TCP response: %v", err)
	}

	// Check the response
	response := string(buffer[:n])
	expectedResponse := "Hello from backend server: " + backendID_TCP
	if !strings.Contains(response, expectedResponse) {
		t.Errorf("Expected TCP response to contain %q, got %q", expectedResponse, response)
	}
}

// --- TestAllBackendsUnhealthy ---
func TestAllBackendsUnhealthy(t *testing.T) {
	// Create a simple pool with one unhealthy backend
	be := &Backend{Addr: "127.0.0.1:9999", Weight: 1}
	be.SetHealth(false)
	pool := &BackendPool{
		backends: []*Backend{be},
		strategy: "round-robin",
	}

	// L7 (HTTP) Test
	t.Run("L7_HTTP", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://example.com", nil)
		rr := httptest.NewRecorder()

		// Create a logger and put it in context (like ServeHTTP does)
		logger := slog.Default()
		ctx := context.WithValue(req.Context(), loggerKey, logger)

		// Manually create the handler
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			backend := pool.GetNextBackend(r, r.RemoteAddr)
			if backend == nil {
				http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
				return
			}
			//... we should never get here
		})

		handler.ServeHTTP(rr, req.WithContext(ctx))

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("Expected status %d when all backends are unhealthy, got %d", http.StatusServiceUnavailable, rr.Code)
		}
	})

	// L4 (TCP) Test
	t.Run("L4_TCP", func(t *testing.T) {
		// For L4, we just check if GetNextBackend returns nil
		backend := pool.GetNextBackend(nil, "127.0.0.1")
		if backend != nil {
			t.Error("Expected GetNextBackend to return nil when all backends are unhealthy")
		}
	})
}

// --- TestL4_UDPProxy ---
func TestL4_UDPProxy(t *testing.T) {
	conn, err := net.Dial("udp", lbUdpAddr)
	if err != nil {
		t.Fatalf("Failed to dial UDP load balancer: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Warn("Failed to close connection", "error", err)
		}
	}()

	msg := []byte("Hello UDP")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("Failed to write UDP packet: %v", err)
	}

	buffer := make([]byte, 1024)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		// In a test, you might want to fail the test
		t.Fatalf("Failed to set read deadline: %v", err) 
		// In real code (like udp_proxy.go), you'd log and return
		// slog.Warn("Failed to set read deadline", "error", err)
		// return 
	}
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("Failed to read UDP response: %v", err)
	}

	expectedResponse := "Hello from UDP server: " + backendID_UDP
	if string(buffer[:n]) != expectedResponse {
		t.Errorf("Expected UDP response %q, got %q", expectedResponse, string(buffer[:n]))
	}
}

func TestL7Routing(t *testing.T) {
	reqWeb, _ := http.NewRequest("GET", "https://"+lbAddr+"/", nil)
	respWeb, err := testClient.Do(reqWeb)
	if err != nil {
		t.Fatalf("Failed default request: %v", err)
	}
	defer func() { _ = respWeb.Body.Close() }()
	bodyWeb, _ := io.ReadAll(respWeb.Body)
	if !strings.Contains(string(bodyWeb), backendID_Web) {
		t.Errorf("Expected default request to hit %s, got %q", backendID_Web, string(bodyWeb))
	}
	reqAPI, _ := http.NewRequest("GET", "https://"+lbAddr+"/users", nil)
	reqAPI.Host = "api.example.com"
	respAPI, err := testClient.Do(reqAPI)
	if err != nil {
		t.Fatalf("Failed Host request: %v", err)
	}
	defer func() { _ = respAPI.Body.Close() }()
	bodyAPI, _ := io.ReadAll(respAPI.Body)
	if !strings.Contains(string(bodyAPI), backendID_API) {
		t.Errorf("Expected Host request to hit %s, got %q", backendID_API, string(bodyAPI))
	}
	reqMob, _ := http.NewRequest("GET", "https://"+lbAddr+"/", nil)
	reqMob.Header.Set("User-Agent", "MobileApp")
	respMob, err := testClient.Do(reqMob)
	if err != nil {
		t.Fatalf("Failed Header request: %v", err)
	}
	defer func() { _ = respMob.Body.Close() }()
	bodyMob, _ := io.ReadAll(respMob.Body)
	if !strings.Contains(string(bodyMob), backendID_Mob) {
		t.Errorf("Expected Header request to hit %s, got %q", backendID_Mob, string(bodyMob))
	}
	reqBoth, _ := http.NewRequest("GET", "https://"+lbAddr+"/", nil)
	reqBoth.Host = "api.example.com"
	reqBoth.Header.Set("User-Agent", "MobileApp")
	respBoth, err := testClient.Do(reqBoth)
	if err != nil {
		t.Fatalf("Failed precedence request: %v", err)
	}
	defer func() { _ = respBoth.Body.Close() }()
	bodyBoth, _ := io.ReadAll(respBoth.Body)
	if !strings.Contains(string(bodyBoth), backendID_API) {
		t.Errorf("Expected precedence request to hit API server %s, got %q", backendID_API, string(bodyBoth))
	}
}
func TestBackendHealth(t *testing.T) {
	b := &Backend{}
	if b.IsHealthy() {
		t.Error("Backend should be unhealthy by default")
	}
	b.SetHealth(true)
	if !b.IsHealthy() {
		t.Error("Backend should be healthy after setting to true")
	}
	b.SetHealth(false)
	if b.IsHealthy() {
		t.Error("Backend should be unhealthy after setting to false")
	}
}
func TestGetNextBackendByLeastConns(t *testing.T) {
	b1 := &Backend{Addr: "server1"}
	b2 := &Backend{Addr: "server2"}
	b3 := &Backend{Addr: "server3"}
	b1.SetHealth(true)
	b1.IncrementConnections()
	b1.IncrementConnections()
	b2.SetHealth(true)
	b3.SetHealth(true)
	b3.IncrementConnections()
	pool := &BackendPool{backends: []*Backend{b1, b2, b3}}
	be := pool.GetNextBackendByLeastConns()
	if be.Addr != "server2" {
		t.Errorf("Expected backend server2, but got %s", be.Addr)
	}
}
func TestGetNextBackendByIP(t *testing.T) {
	b1 := &Backend{Addr: "server1"}
	b2 := &Backend{Addr: "server2"}
	b3 := &Backend{Addr: "server3"}
	pool := &BackendPool{backends: []*Backend{b1, b2, b3}}
	b1.SetHealth(true)
	b2.SetHealth(true)
	b3.SetHealth(true)
	ip1 := "192.168.1.1"
	be1 := pool.GetNextBackendByIP(ip1)
	be3 := pool.GetNextBackendByIP(ip1)
	if be1.Addr != be3.Addr {
		t.Errorf("IP Hashing failed: same IP got different backends")
	}
}
func TestGetNextBackendByRoundRobin(t *testing.T) {
	b1 := &Backend{Addr: "server1"}
	b2 := &Backend{Addr: "server2"}
	b3 := &Backend{Addr: "server3"}
	pool := &BackendPool{backends: []*Backend{b1, b2, b3}}
	b1.SetHealth(true)
	b2.SetHealth(false)
	b3.SetHealth(true)
	pool.current = 1
	expectedAddrs := []string{"server3", "server1", "server3", "server1"}
	for _, expected := range expectedAddrs {
		backend := pool.GetNextBackendByRoundRobin()
		if backend.Addr != expected {
			t.Errorf("Expected backend %s, but got %s", expected, backend.Addr)
		}
	}
}
func TestGetNextBackendByWeightedRoundRobin(t *testing.T) {
	b1 := &Backend{Addr: "server1", Weight: 5}
	b2 := &Backend{Addr: "server2", Weight: 1}
	b3 := &Backend{Addr: "server3", Weight: 1}
	pool := &BackendPool{backends: []*Backend{b1, b2, b3}}
	b1.SetHealth(true)
	b2.SetHealth(true)
	b3.SetHealth(true)
	expectedAddrs := []string{"server1", "server1", "server2", "server1", "server3", "server1", "server1"}
	counts := make(map[string]int)
	for _, expected := range expectedAddrs {
		backend := pool.GetNextBackendByWeightedRoundRobin()
		if backend.Addr != expected {
			t.Errorf("Expected backend %s, but got %s", expected, backend.Addr)
		}
		counts[backend.Addr]++
	}
	if counts["server1"] != 5 || counts["server2"] != 1 || counts["server3"] != 1 {
		t.Errorf("Incorrect distribution: got %v", counts)
	}
}
func TestGetNextBackendByWeightedLeastConns(t *testing.T) {
	b1 := &Backend{Addr: "server1", Weight: 5}
	b2 := &Backend{Addr: "server2", Weight: 1}
	b3 := &Backend{Addr: "server3", Weight: 1}
	pool := &BackendPool{backends: []*Backend{b1, b2, b3}}
	b1.SetHealth(true)
	b2.SetHealth(true)
	b3.SetHealth(true)
	be := pool.GetNextBackendByWeightedLeastConns()
	if be.Addr != "server1" {
		t.Errorf("Expected server1 (first in list), got %s", be.Addr)
	}
	b1.IncrementConnections()
	b1.IncrementConnections()
	b1.IncrementConnections()
	b1.IncrementConnections()
	b1.IncrementConnections()
	b2.IncrementConnections()
	b2.IncrementConnections()
	b3.IncrementConnections()
	be = pool.GetNextBackendByWeightedLeastConns()
	if be.Addr != "server1" {
		t.Errorf("Expected server1 (score 1.0), got %s (score %f)", be.Addr, float64(be.GetConnections())/float64(be.Weight))
	}
	b1.IncrementConnections()
	be = pool.GetNextBackendByWeightedLeastConns()
	if be.Addr != "server3" {
		t.Errorf("Expected server3 (score 1.0), got %s (score %f)", be.Addr, float64(be.GetConnections())/float64(be.Weight))
	}
}
func TestMetricsServer(t *testing.T) {
	resp, err := http.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		t.Fatalf("Failed to make request to metrics server: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read metrics response body: %v", err)
	}
	bodyStr := string(body)
	expectedMetrics := []string{
		fmt.Sprintf("backend_health_status{backend=\"%s\", route_path=\"/\", route_host=\"api.example.com\"} 1", backendAddrAPI),
		fmt.Sprintf("backend_active_connections{backend=\"%s\", route_path=\"/\", route_host=\"api.example.com\"} 0", backendAddrAPI),
		fmt.Sprintf("backend_health_status{backend=\"%s\", route_path=\"/\", route_host=\"\"} 1", backendAddrMob),
		fmt.Sprintf("backend_health_status{backend=\"%s\", route_path=\"/\", route_host=\"\"} 1", backendAddrWeb),
		fmt.Sprintf("backend_health_status{backend=\"%s\", listener_name=\"integration-test-udp\"} 1", backendAddrUDP),
		fmt.Sprintf("backend_health_status{backend=\"%s\", listener_name=\"integration-test-tcp\"} 1", backendAddrTCP),
	}
	for _, expected := range expectedMetrics {
		if !strings.Contains(bodyStr, expected) {
			t.Errorf("Metrics response missing expected line. Got:\n%s\nExpected to contain:\n%s", bodyStr, expected)
		}
	}
}
