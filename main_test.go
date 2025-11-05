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
			conn.Close()
			return nil // Success!
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// TestMain sets up and tears down the servers for testing.
func TestMain(m *testing.M) {
	// 1. Start the backend server
	backendListener, err := backend.RunServer("9001", backendID)
	if err != nil {
		log.Fatalf("Failed to start backend server: %v", err)
	}
	defer backendListener.Close()

	// 2. Start the load balancer
	go func() {
		// We expect a net.ErrClosed, so we check for it
		if err := RunLoadBalancer(":8080", backendAddr); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("LB exited: %v", err)
		}
	}()

	// 3. Wait for servers to be ready (replaces time.Sleep)
	if err := waitForPort(backendAddr, 2*time.Second); err != nil {
		log.Fatalf("Backend server failed to start: %v", err)
	}
	if err := waitForPort(lbAddr, 2*time.Second); err != nil {
		log.Fatalf("Load balancer failed to start: %v", err)
	}

	// 4. Run the tests
	m.Run()
}

// TestProxyRequest is the INTEGRATION TEST.
func TestProxyRequest(t *testing.T) {
	// 1. Make a request to the load balancer
	resp, err := http.Get("http://" + lbAddr)
	if err != nil {
		t.Fatalf("Failed to make request to load balancer: %v", err)
	}
	defer resp.Body.Close()

	// 2. Read the response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	// 3. Verify the response
	expectedResponse := "Hello from backend server: " + backendID
	if !strings.Contains(string(body), expectedResponse) {
		t.Errorf("Unexpected response body. Got %q, expected to contain %q", string(body), expectedResponse)
	}
}

// TestHandleProxy_Unit is the UNIT TEST.
func TestHandleProxy_Unit(t *testing.T) {
	// 1. Setup
	// Create two pairs of "in-memory" connections
	clientConn, clientPipe := net.Pipe()
	backendConn, backendPipe := net.Pipe()

	// 2. Run the function we are testing in a goroutine
	go handleProxy(clientPipe, backendPipe)

	// 3. Run the "fake" backend in a goroutine
	go func() {
		defer backendConn.Close()
		// Read "hello" from the proxy
		buf := make([]byte, 5)
		if _, err := backendConn.Read(buf); err != nil {
			t.Errorf("Fake backend failed to read: %v", err)
			return
		}

		if string(buf) != "hello" {
			t.Errorf("Fake backend expected 'hello', got %q", string(buf))
			return
		}

		// Write "world" back to the proxy
		if _, err := backendConn.Write([]byte("world")); err != nil {
			t.Errorf("Fake backend failed to write: %v", err)
		}
	}()

	// 4. Run the "fake" client (in this main test goroutine)
	if _, err := clientConn.Write([]byte("hello")); err != nil {
		t.Fatalf("Client write error: %v", err)
	}

	// Read the response from the proxy
	buf := make([]byte, 5) // "world"
	if _, err := clientConn.Read(buf); err != nil {
		t.Fatalf("Client read error: %v", err)
	}

	if string(buf) != "world" {
		t.Fatalf("Client expected 'world', got %q", string(buf))
	}

	// Clean up
	clientConn.Close()
}
