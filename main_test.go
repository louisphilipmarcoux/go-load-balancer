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
	backendAddr = "localhost:9001" // This now refers to our *one* test backend
	backendID   = "Test-Server-1"
)

// ... waitForPort function (no changes) ...
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

// CHANGED: This function is updated to use the new pool
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

	// CHANGED: Create a test pool pointing to our *one* test backend
	testPool := &BackendPool{
		backends: []*Backend{
			{Addr: backendAddr}, // backendAddr is "localhost:9001"
		},
	}

	// CHANGED: Call RunLoadBalancer with the new pool
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

// ... TestProxyRequest function (no changes) ...
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

// ... TestHandleProxy_Unit function (no changes) ...
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
