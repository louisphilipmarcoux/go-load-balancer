package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux" // NEW: Import gorilla/mux
)

// newTestLoadBalancer creates a minimal LB for testing handlers.
func newTestLoadBalancer() *LoadBalancer {
	cfg := &Config{
		AdminToken: "test-token",
		Routes: []*RouteConfig{
			{Path: "/", Backends: []*BackendConfig{{Addr: "localhost:9000", Weight: 1}}},
		},
	}
	lb := NewLoadBalancer(cfg)
	return lb
}

func TestAdminAuthMiddleware(t *testing.T) {
	lb := newTestLoadBalancer()

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// FIX: Check the error returned by w.Write()
		if _, err := w.Write([]byte("OK")); err != nil {
			// In a test handler, you might log or ignore, but the linter is satisfied.
			// Since this is a test, we simply return without further error handling.
			return
		}
	})

	authMiddleware := adminAuthMiddleware(lb)
	handlerToTest := authMiddleware(nextHandler)

	t.Run("No token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		rr := httptest.NewRecorder()
		handlerToTest.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("Wrong token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		rr := httptest.NewRecorder()
		handlerToTest.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("Correct token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		handlerToTest.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if rr.Body.String() != "OK" {
			t.Error("Middleware did not call next handler")
		}
	})
}

func TestGetRoutesHandler(t *testing.T) {
	lb := newTestLoadBalancer()
	req := httptest.NewRequest("GET", "/api/v1/routes", nil)
	rr := httptest.NewRecorder()

	handler := getRoutesHandler(lb)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var routes []*RouteConfig
	if err := json.NewDecoder(rr.Body).Decode(&routes); err != nil {
		t.Fatalf("Failed to decode JSON response: %v", err)
	}

	if len(routes) != 1 {
		t.Errorf("Expected 1 route, got %d", len(routes))
	}
	if routes[0].Backends[0].Addr != "localhost:9000" {
		t.Errorf("Expected backend 'localhost:9000', got %s", routes[0].Backends[0].Addr)
	}
}

func TestAddBackendHandler(t *testing.T) {
	lb := newTestLoadBalancer()

	// This is our test JSON payload
	body := `{"addr": "localhost:9001", "weight": 2}`
	req := httptest.NewRequest("POST", "/api/v1/routes/0/backends", strings.NewReader(body))
	rr := httptest.NewRecorder()

	// --- THIS IS THE FIX ---
	// We must use gorilla/mux here, just like in admin.go
	r := mux.NewRouter()

	// Register the handler with the correct path syntax
	// We've wrapped our handler in a subrouter in admin.go, but for a unit test
	// we can just register the handler directly.
	r.HandleFunc("/api/v1/routes/{index:[0-9]+}/backends", addBackendHandler(lb)).Methods("POST")
	// --- END OF FIX ---

	// Now, when we serve, gorilla/mux will parse "0" into the vars
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("Expected status %d, got %d. Body: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	// Check if the LB's config was actually updated
	lb.lock.RLock()
	defer lb.lock.RUnlock()
	if len(lb.cfg.Routes[0].Backends) != 2 {
		t.Errorf("Expected 2 backends, got %d", len(lb.cfg.Routes[0].Backends))
	}
	if lb.cfg.Routes[0].Backends[1].Addr != "localhost:9001" {
		t.Error("New backend was not added")
	}
}
