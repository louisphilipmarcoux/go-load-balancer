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

// waitForPort polls a TCP address until it's available or a timeout is reached.
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

// TestMain sets up and tears down the servers for testing.
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

	testPool := &BackendPool{
		backends: []*Backend{
			{Addr: backendAddr},
		},
	}

	// Manually set the backend to healthy for the integration test
	// Otherwise, GetNextBackend() will return nil
	testPool.backends[0].SetHealth(true)

	go func() {
		if err := RunLoadBalancer(":8080", testPool); err != nil && !errors.Is(err, net.ErrClosed) {
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

// TestProxyRequest is the INTEGRATION TEST.
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

// TestHandleProxy_Unit is the UNIT TEST.
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

// TestHealthAwareRoundRobin is the unit test for Stage 6 logic
func TestHealthAwareRoundRobin(t *testing.T) {
	// 1. Setup our test pool
	b1 := &Backend{Addr: "server1"}
	b2 := &Backend{Addr: "server2"}
	b3 := &Backend{Addr: "server3"}
	pool := &BackendPool{
		backends: []*Backend{b1, b2, b3},
	}

	// 2. Set initial health: b1 and b3 are UP, b2 is DOWN
	b1.SetHealth(true)
	b2.SetHealth(false)
	b3.SetHealth(true)

	// 3. Define the expected sequence:
	// We are removing the ineffectual assignment. This is the only line now.
	// Logic:
	// 1st call: p.current=1, index 1 (b2) is DOWN. p.current=2, index 2 (b3) is UP. Return b3.
	// 2nd call: p.current=3, index 0 (b1) is UP. Return b1.
	// 3rd call: p.current=4, index 1 (b2) is DOWN. p.current=5, index 2 (b3) is UP. Return b3.
	// 4th call: p.current=6, index 0 (b1) is UP. Return b1.
	expectedAddrs := []string{"server3", "server1", "server3", "server1"}

	for _, expected := range expectedAddrs {
		backend := pool.GetNextBackend()
		if backend.Addr != expected {
			t.Errorf("Expected backend %s, but got %s", expected, backend.Addr)
		}
	}

	// 4. Test "all down" scenario
	b1.SetHealth(false)
	b3.SetHealth(false)
	if backend := pool.GetNextBackend(); backend != nil {
		t.Error("Expected nil backend when all are down")
	}

	// 5. Test recovery
	b2.SetHealth(true) // Now only b2 is UP
	if backend := pool.GetNextBackend(); backend.Addr != "server2" {
		t.Errorf("Expected server2 after recovery, got %s", backend.Addr)
	}
}

// TestBackendHealth is the unit test for Stage 5 logic
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
