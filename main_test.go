package main

import (
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

// --- CHANGED THIS BLOCK ---
// Add a const for the test metrics server
const (
	lbAddr      = "localhost:8080"
	backendAddr = "localhost:9099"
	metricsAddr = "localhost:9090" // NEW
	backendID   = "Test-Server-1"
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

// --- CHANGED: Update TestMain to use LoadBalancer struct ---
func TestMain(m *testing.M) {
	backendListener, err := backend.RunServer("9099", backendID)
	if err != nil {
		log.Fatalf("Failed to start backend server: %v", err)
	}
	defer func() {
		if err := backendListener.Close(); err != nil {
			log.Printf("Warning: failed to close backend listener: %v", err)
		}
	}()

	cfg := &Config{
		ListenAddr:  lbAddr,
		MetricsAddr: metricsAddr,
		Strategy:    "round-robin",
		Backends:    []*BackendConfig{{Addr: backendAddr, Weight: 1}},
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("Failed to create listener for test: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			log.Printf("Warning: failed to close test listener: %v", err)
		}
	}()

	// Create the new LoadBalancer struct
	lb := NewLoadBalancer(cfg, listener)
	// Manually set health for the test, since we're not running StartHealthChecks
	lb.pool.backends[0].SetHealth(true)

	// Start the metrics server
	go StartMetricsServer(cfg.MetricsAddr, lb)

	// Start the main LB run loop
	go func() {
		if err := lb.Run(); err != nil {
			log.Printf("LB exited: %v", err)
		}
	}()

	// Wait for all servers
	if err := waitForPort(backendAddr, 2*time.Second); err != nil {
		log.Fatalf("Backend server failed to start: %v", err)
	}
	if err := waitForPort(lbAddr, 2*time.Second); err != nil {
		log.Fatalf("Load balancer failed to start: %v", err)
	}
	if err := waitForPort(metricsAddr, 2*time.Second); err != nil {
		log.Fatalf("Metrics server failed to start: %v", err)
	}

	m.Run()
}

// ... (All other tests remain the same) ...
func TestProxyRequest(t *testing.T) {
	resp, err := http.Get("http://" + lbAddr)
	if err != nil {
		t.Fatalf("Failed to make request to load balancer: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("Warning: failed to close response body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	expectedResponse := "Hello from backend server: " + backendID
	if !strings.Contains(string(body), expectedResponse) {
		t.Errorf("Unexpected response body. Got %q, expected to contain %q", string(body), expectedResponse)
	}
}
func TestHandleProxy_Unit(t *testing.T) {
	clientConn, clientPipe := net.Pipe()
	backendConn, backendPipe := net.Pipe()
	go handleProxy(clientPipe, backendPipe)
	go func() {
		defer func() {
			if err := backendConn.Close(); err != nil {
				t.Logf("Warning: failed to close backend pipe: %v", err)
			}
		}()
		buf := make([]byte, 5)
		if _, err := backendConn.Read(buf); err != nil {
			t.Errorf("Fake backend failed to read: %v", err)
			return
		}
		if string(buf) != "hello" {
			t.Errorf("Fake backend expected 'hello', got %q", string(buf))
		}
		if _, err := backendConn.Write([]byte("world")); err != nil {
			t.Errorf("Fake backend failed to write: %v", err)
		}
	}()
	if _, err := clientConn.Write([]byte("hello")); err != nil {
		t.Fatalf("Client write error: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := clientConn.Read(buf); err != nil {
		t.Fatalf("Client read error: %v", err)
	}
	if string(buf) != "world" {
		t.Fatalf("Client expected 'world', got %q", string(buf))
	}
	if err := clientConn.Close(); err != nil {
		t.Logf("Warning: failed to close client pipe: %v", err)
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

// NEW: Test for the metrics server
func TestMetricsServer(t *testing.T) {
	// 1. Make a request to the metrics server
	resp, err := http.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		t.Fatalf("Failed to make request to metrics server: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Logf("Warning: failed to close metrics response body: %v", err)
		}
	}()

	// 2. Read the response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read metrics response body: %v", err)
	}

	// 3. Verify the response
	// We're testing against the *test* server (Test-Server-1 on port 9099)
	// which we set to healthy=true in TestMain.
	expectedMetrics := []string{
		"total_active_connections 0",
		fmt.Sprintf("backend_health_status{backend=\"%s\"} 1", backendAddr),
		fmt.Sprintf("backend_active_connections{backend=\"%s\"} 0", backendAddr),
	}

	for _, expected := range expectedMetrics {
		if !strings.Contains(string(body), expected) {
			t.Errorf("Metrics response missing expected line. Got:\n%s\nExpected to contain:\n%s",
				string(body), expected)
		}
	}
}
