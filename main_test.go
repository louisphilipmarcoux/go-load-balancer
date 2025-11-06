package main

import (
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

const (
	lbAddr      = "localhost:8080"
	backendAddr = "localhost:9001"
	backendID   = "Test-Server-1"
)

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

func TestMain(m *testing.M) {
	backendListener, err := backend.RunServer("9001", backendID)
	if err != nil {
		log.Fatalf("Failed to start backend server: %v", err)
	}
	defer func() {
		if err := backendListener.Close(); err != nil {
			log.Printf("Warning: failed to close backend listener: %v", err)
		}
	}()

	cfg := &Config{
		ListenAddr: ":8080",
		Strategy:   "round-robin",
		Backends:   []*BackendConfig{{Addr: backendAddr, Weight: 1}},
	}
	testPool := NewBackendPool(cfg)
	testPool.backends[0].SetHealth(true)

	go func() {
		if err := RunLoadBalancer(cfg, testPool); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("LB exited: %v", err)
		}
	}()

	if err := waitForPort(backendAddr, 2*time.Second); err != nil {
		log.Fatalf("Backend server failed to start: %v", err)
	}
	if err := waitForPort(lbAddr, 2*time.Second); err != nil {
		log.Fatalf("Load balancer failed to start: %v", err)
	}

	m.Run()
}

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

// Test for Weighted Round Robin
// Test for Weighted Round Robin
func TestGetNextBackendByWeightedRoundRobin(t *testing.T) {
	// 1. Setup pool with weights 5, 1, 1
	b1 := &Backend{Addr: "server1", Weight: 5}
	b2 := &Backend{Addr: "server2", Weight: 1}
	b3 := &Backend{Addr: "server3", Weight: 1}
	pool := &BackendPool{
		backends: []*Backend{b1, b2, b3},
	}
	b1.SetHealth(true)
	b2.SetHealth(true)
	b3.SetHealth(true)

	// 2. Expected distribution over 7 requests (5+1+1)
	// This is the correct trace of the algorithm.
	expectedAddrs := []string{"server1", "server1", "server2", "server1", "server3", "server1", "server1"}

	counts := make(map[string]int)
	for _, expected := range expectedAddrs {
		backend := pool.GetNextBackendByWeightedRoundRobin()
		if backend.Addr != expected {
			// This line is line 232 in your file
			t.Errorf("Expected backend %s, but got %s", expected, backend.Addr)
		}
		counts[backend.Addr]++
	}

	if counts["server1"] != 5 || counts["server2"] != 1 || counts["server3"] != 1 {
		t.Errorf("Incorrect distribution: got %v", counts)
	}
}
