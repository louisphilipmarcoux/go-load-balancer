package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http" // NEW
	"strings"

	// "sync" // No longer needed
	"testing"
	"time"

	"context" // NEW

	"github.com/louisphilipmarcoux/go-load-balancer/backend"
)

// --- CHANGED THIS BLOCK ---
// We now need multiple backends for L7 routing tests
const (
	lbAddr      = "localhost:8080"
	metricsAddr = "localhost:9090"
	// Backend for /api
	backendAddrAPI = "localhost:9091"
	backendID_API  = "API-Server-1"
	// Backend for /
	backendAddrWeb = "localhost:9092"
	backendID_Web  = "Web-Server-1"
)

// --- END OF CHANGE ---

// ... waitForPort (no changes) ...
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

// CHANGED: TestMain now sets up L7 routing
func TestMain(m *testing.M) {
	// Start API server
	apiListener, err := backend.RunServer("9091", backendID_API)
	if err != nil {
		log.Fatalf("Failed to start API backend server: %v", err)
	}
	defer func() { _ = apiListener.Close() }()

	// Start Web server
	webListener, err := backend.RunServer("9092", backendID_Web)
	if err != nil {
		log.Fatalf("Failed to start Web backend server: %v", err)
	}
	defer func() { _ = webListener.Close() }()

	// Create a test config for L7 routing
	cfg := &Config{
		ListenAddr:  lbAddr,
		MetricsAddr: metricsAddr,
		Routes: []*RouteConfig{
			{
				Path:     "/api/",
				Strategy: "round-robin",
				Backends: []*BackendConfig{{Addr: backendAddrAPI, Weight: 1}},
			},
			{
				Path:     "/",
				Strategy: "round-robin",
				Backends: []*BackendConfig{{Addr: backendAddrWeb, Weight: 1}},
			},
		},
	}

	// Create the new LoadBalancer (which is an http.Handler)
	lb := NewLoadBalancer(cfg)

	// Manually set health for the test pools
	lb.pools["/api/"].backends[0].SetHealth(true)
	lb.pools["/"].backends[0].SetHealth(true)

	// Create and start the main LB server
	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: lb,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("LB server exited: %v", err)
		}
	}()
	defer func() { _ = server.Shutdown(context.Background()) }()

	// Start the metrics server
	go StartMetricsServer(cfg.MetricsAddr, lb)

	// Wait for all 4 servers
	if err := waitForPort(backendAddrAPI, 2*time.Second); err != nil {
		log.Fatalf("API Backend server failed to start: %v", err)
	}
	if err := waitForPort(backendAddrWeb, 2*time.Second); err != nil {
		log.Fatalf("Web Backend server failed to start: %v", err)
	}
	if err := waitForPort(lbAddr, 2*time.Second); err != nil {
		log.Fatalf("Load balancer failed to start: %v", err)
	}
	if err := waitForPort(metricsAddr, 2*time.Second); err != nil {
		log.Fatalf("Metrics server failed to start: %v", err)
	}

	m.Run()
}

// NEW: TestL7Routing replaces TestProxyRequest
func TestL7Routing(t *testing.T) {
	// Test 1: Request to /api/users should go to API server
	respAPI, err := http.Get("http://" + lbAddr + "/api/users")
	if err != nil {
		t.Fatalf("Failed to make /api request: %v", err)
	}
	defer func() { _ = respAPI.Body.Close() }()
	bodyAPI, _ := io.ReadAll(respAPI.Body)

	if !strings.Contains(string(bodyAPI), backendID_API) {
		t.Errorf("Expected /api request to hit %s, got %q", backendID_API, string(bodyAPI))
	}

	// Test 2: Request to / should go to Web server
	respWeb, err := http.Get("http://" + lbAddr + "/")
	if err != nil {
		t.Fatalf("Failed to make / request: %v", err)
	}
	defer func() { _ = respWeb.Body.Close() }()
	bodyWeb, _ := io.ReadAll(respWeb.Body)

	if !strings.Contains(string(bodyWeb), backendID_Web) {
		t.Errorf("Expected / request to hit %s, got %q", backendID_Web, string(bodyWeb))
	}

	// Test 3: Request to /foo should also go to Web server
	respFoo, err := http.Get("http://" + lbAddr + "/foo")
	if err != nil {
		t.Fatalf("Failed to make /foo request: %v", err)
	}
	defer func() { _ = respFoo.Body.Close() }()
	bodyFoo, _ := io.ReadAll(respFoo.Body)

	if !strings.Contains(string(bodyFoo), backendID_Web) {
		t.Errorf("Expected /foo request to hit %s, got %q", backendID_Web, string(bodyFoo))
	}
}

// GONE: TestHandleProxy_Unit is no longer relevant

// ... (TestBackendHealth, LeastConns, IP, RoundRobin, Weighted... are all UNCHANGED) ...
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

// CHANGED: TestMetricsServer now checks for new L7 labels
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

	// 3. Verify the response
	expectedMetrics := []string{
		"total_active_connections 0",
		// API Server
		fmt.Sprintf("backend_health_status{backend=\"%s\", route=\"/api/\"} 1", backendAddrAPI),
		fmt.Sprintf("backend_active_connections{backend=\"%s\", route=\"/api/\"} 0", backendAddrAPI),
		// Web Server
		fmt.Sprintf("backend_health_status{backend=\"%s\", route=\"/\"} 1", backendAddrWeb),
		fmt.Sprintf("backend_active_connections{backend=\"%s\", route=\"/\"} 0", backendAddrWeb),
	}

	for _, expected := range expectedMetrics {
		if !strings.Contains(string(body), expected) {
			t.Errorf("Metrics response missing expected line. Got:\n%s\nExpected to contain:\n%s",
				string(body), expected)
		}
	}
}
