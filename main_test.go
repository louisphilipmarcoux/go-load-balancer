package main

import (
	"context"
	"crypto/tls" // NEW
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/louisphilipmarcoux/go-load-balancer/backend"
)

// We now need 3 backends for testing
const (
	lbAddr      = "localhost:8443" // Test on the TLS port
	metricsAddr = "localhost:9090"

	backendAddrAPI = "localhost:9091"
	backendID_API  = "API-Server-1"
	backendAddrMob = "localhost:9092"
	backendID_Mob  = "Mobile-Server-1"
	backendAddrWeb = "localhost:9093"
	backendID_Web  = "Web-Server-1"
)

// Create a test http client that trusts our self-signed cert
var testClient *http.Client

// waitForPort waits for a port to become available
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

// TestMain now sets up the advanced routing test
func TestMain(m *testing.M) {
	// Start API server (for api.example.com)
	apiListener, err := backend.RunServer("9091", backendID_API)
	if err != nil {
		log.Fatalf("Failed to start API backend: %v", err)
	}
	defer func() { _ = apiListener.Close() }()

	// Start Mobile server (for User-Agent: MobileApp)
	mobListener, err := backend.RunServer("9092", backendID_Mob)
	if err != nil {
		log.Fatalf("Failed to start Mobile backend: %v", err)
	}
	defer func() { _ = mobListener.Close() }()

	// Start Web server (default)
	webListener, err := backend.RunServer("9093", backendID_Web)
	if err != nil {
		log.Fatalf("Failed to start Web backend: %v", err)
	}
	defer func() { _ = webListener.Close() }()

	// Create a test config for L7 routing
	cfg := &Config{
		ListenAddr:  lbAddr,
		MetricsAddr: metricsAddr,
		TLS: &TLSConfig{ // Enable TLS for the test
			CertFile: "server.crt",
			KeyFile:  "server.key",
		},
		// Note: No RateLimit or CircuitBreaker config for simplicity in tests
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
	}

	lb := NewLoadBalancer(cfg)
	// Manually set health for test pools
	lb.routes[0].pool.backends[0].SetHealth(true)
	lb.routes[1].pool.backends[0].SetHealth(true)
	lb.routes[2].pool.backends[0].SetHealth(true)

	server := &http.Server{Addr: cfg.ListenAddr, Handler: lb}
	go func() {
		if err := server.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("LB server exited: %v", err)
		}
	}()
	defer func() { _ = server.Shutdown(context.Background()) }()

	go StartMetricsServer(cfg.MetricsAddr, lb)

	// Wait for all servers
	ports := []string{backendAddrAPI, backendAddrMob, backendAddrWeb, lbAddr, metricsAddr}
	for _, port := range ports {
		if err := waitForPort(port, 2*time.Second); err != nil {
			log.Fatalf("Server on %s failed to start: %v", port, err)
		}
	}

	// Create the test client
	testClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Trust self-signed cert
		},
	}

	m.Run()
}

// TestL7Routing now tests Host and Header routing
func TestL7Routing(t *testing.T) {
	// Test 1: Default request -> Web Server
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

	// Test 2: Host header -> API Server
	reqAPI, _ := http.NewRequest("GET", "https://"+lbAddr+"/users", nil)
	reqAPI.Host = "api.example.com" // Set the Host
	respAPI, err := testClient.Do(reqAPI)
	if err != nil {
		t.Fatalf("Failed Host request: %v", err)
	}
	defer func() { _ = respAPI.Body.Close() }()
	bodyAPI, _ := io.ReadAll(respAPI.Body)
	if !strings.Contains(string(bodyAPI), backendID_API) {
		t.Errorf("Expected Host request to hit %s, got %q", backendID_API, string(bodyAPI))
	}

	// Test 3: Header -> Mobile Server
	reqMob, _ := http.NewRequest("GET", "https://"+lbAddr+"/", nil)
	reqMob.Header.Set("User-Agent", "MobileApp") // Set the Header
	respMob, err := testClient.Do(reqMob)
	if err != nil {
		t.Fatalf("Failed Header request: %v", err)
	}
	defer func() { _ = respMob.Body.Close() }()
	bodyMob, _ := io.ReadAll(respMob.Body)
	if !strings.Contains(string(bodyMob), backendID_Mob) {
		t.Errorf("Expected Header request to hit %s, got %q", backendID_Mob, string(bodyMob))
	}

	// Test 4: Host header should take precedence over Header
	// (Our routes are in order: Host, Header, Path. So Host will match first)
	reqBoth, _ := http.NewRequest("GET", "https://"+lbAddr+"/", nil)
	reqBoth.Host = "api.example.com"              // API Host
	reqBoth.Header.Set("User-Agent", "MobileApp") // Mobile Header
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

// --- Unit Tests (Unchanged) ---
// These test the components in isolation

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
	pool := &BackendPool{
		backends: []*Backend{b1, b2, b3},
	}
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
	pool := &BackendPool{
		backends: []*Backend{b1, b2, b3},
	}
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
		t.Errorf("Expected server1 (score 1.0), got %s (score %f)",
			be.Addr, float64(be.GetConnections())/float64(be.Weight))
	}
	b1.IncrementConnections()
	be = pool.GetNextBackendByWeightedLeastConns()
	if be.Addr != "server3" {
		t.Errorf("Expected server3 (score 1.0), got %s (score %f)",
			be.Addr, float64(be.GetConnections())/float64(be.Weight))
	}
}

// TestMetricsServer checks the metrics endpoint
// This test is slightly less critical now, but still good to have.
// It uses the config from TestMain.
// TestMetricsServer checks the metrics endpoint
func TestMetricsServer(t *testing.T) {
	// CHANGED: Use a standard http.Get, not the testClient.
	// The metrics server is on HTTP, not HTTPS.
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

	// 3. Verify the response
	expectedMetrics := []string{
		"total_active_connections 0",
		// API Server
		fmt.Sprintf("backend_health_status{backend=\"%s\", route_path=\"/\", route_host=\"api.example.com\"} 1", backendAddrAPI),
		fmt.Sprintf("backend_active_connections{backend=\"%s\", route_path=\"/\", route_host=\"api.example.com\"} 0", backendAddrAPI),
		// Mobile Server
		fmt.Sprintf("backend_health_status{backend=\"%s\", route_path=\"/\", route_host=\"\"} 1", backendAddrMob),
		// Web Server
		fmt.Sprintf("backend_health_status{backend=\"%s\", route_path=\"/\", route_host=\"\"} 1", backendAddrWeb),
	}

	for _, expected := range expectedMetrics {
		if !strings.Contains(bodyStr, expected) {
			t.Errorf("Metrics response missing expected line. Got:\n%s\nExpected to contain:\n%s",
				bodyStr, expected)
		}
	}
}
