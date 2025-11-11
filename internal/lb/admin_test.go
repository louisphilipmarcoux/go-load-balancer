package lb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// newTestLoadBalancer creates a minimal LB for testing handlers.
func newTestLoadBalancer() *LoadBalancer {
	cfg := &Config{
		AdminToken: "test-token",
		Listeners: []*ListenerConfig{
			{
				Name:     "test-listener",
				Protocol: "http",
				Routes: []*RouteConfig{
					{Path: "/", Backends: []*BackendConfig{{Addr: "localhost:9000", Weight: 1}}},
				},
			},
		},
	}
	lb := NewLoadBalancer(cfg)
	return lb
}

// TestAdminAuthMiddleware
func TestAdminAuthMiddleware(t *testing.T) {
	lb := newTestLoadBalancer()

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
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

// TestGetRoutesHandler
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

// TestAddBackendHandler
func TestAddBackendHandler(t *testing.T) {
	lb := newTestLoadBalancer()

	body := `{"addr": "localhost:9001", "weight": 2}`
	req := httptest.NewRequest("POST", "/api/v1/routes/0/backends", strings.NewReader(body))
	rr := httptest.NewRecorder()

	r := mux.NewRouter()
	r.HandleFunc("/api/v1/routes/{index:[0-9]+}/backends", addBackendHandler(lb)).Methods("POST")
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("Expected status %d, got %d. Body: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	// Check if the LB's config was actually updated
	listener, _ := getFirstL7Listener(lb)
	if len(listener.Routes[0].Backends) != 2 {
		t.Errorf("Expected 2 backends, got %d", len(listener.Routes[0].Backends))
	}
	if listener.Routes[0].Backends[1].Addr != "localhost:9001" {
		t.Error("New backend was not added")
	}
}
