package main

import (
	"errors"
	"fmt"
	"io"
	"log" // NEW: Import math
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/louisphilipmarcoux/go-load-balancer/backend"
)

// ... consts, waitForPort, TestMain, TestProxyRequest, TestHandleProxy_Unit (no changes) ...
// (These tests are all still valid)
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

	testPool := &BackendPool{
		backends: []*Backend{
			{Addr: backendAddr},
		},
	}
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

// CHANGED: This test is renamed and updated to test Least Connections
func TestLeastConnections(t *testing.T) {
	// 1. Setup our test pool
	b1 := &Backend{Addr: "server1"}
	b2 := &Backend{Addr: "server2"}
	b3 := &Backend{Addr: "server3"}

	// Set initial health and connections
	b1.SetHealth(true) // 2 connections
	b1.IncrementConnections()
	b1.IncrementConnections()

	b2.SetHealth(true) // 0 connections

	b3.SetHealth(true) // 1 connection
	b3.IncrementConnections()

	pool := &BackendPool{
		backends: []*Backend{b1, b2, b3},
	}

	// 2. Test initial selection
	// b2 has 0 conns, b3 has 1, b1 has 2.
	// It should pick b2.
	be := pool.GetNextBackend()
	if be.Addr != "server2" {
		t.Errorf("Expected backend server2, but got %s", be.Addr)
	}

	// 3. Test after load is balanced
	// Let's "add" a connection to b2
	be.IncrementConnections() // b2 now has 1 conn

	// b2 has 1 conn, b3 has 1, b1 has 2.
	// It should pick b2 or b3.
	be = pool.GetNextBackend()
	if be.Addr != "server2" && be.Addr != "server3" {
		t.Errorf("Expected backend server2 or server3, but got %s", be.Addr)
	}
	be.IncrementConnections() // b2 or b3 now has 2 conns

	// 4. Test "all down" scenario
	b1.SetHealth(false)
	b2.SetHealth(false)
	b3.SetHealth(false)
	if backend := pool.GetNextBackend(); backend != nil {
		t.Error("Expected nil backend when all are down")
	}
}

// ... TestBackendHealth function (no changes) ...
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
