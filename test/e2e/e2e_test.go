//go:build e2e

package e2e_test // CHANGED: Package name

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	// NEW: Import your load balancer package
	"github.com/louisphilipmarcoux/go-load-balancer/internal/lb"
)

// These constants point to the HOST ports defined in docker-compose.yaml
const (
	e2eHostBase   = "https://127.0.0.1:8443"
	e2eAdminAddr  = "http://127.0.0.1:9091"
	e2eAdminToken = "Fpr4_!$$9qA.8X!u-t_7-G$$x" // From your .env
)

// e2eClient is an HTTP client that trusts our self-signed cert
var e2eClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

// TestE2E_L7_Routing verifies the docker-compose stack routes correctly
func TestE2E_L7_Routing(t *testing.T) {
	// 1. Test the "web-service" route
	reqWeb, _ := http.NewRequest("GET", e2eHostBase+"/", nil)
	respWeb, err := e2eClient.Do(reqWeb)
	if err != nil {
		t.Fatalf("E2E web request failed: %v", err)
	}
	bodyWeb, _ := io.ReadAll(respWeb.Body)
	respWeb.Body.Close()

	if !strings.Contains(string(bodyWeb), "Server-3") {
		t.Errorf("Expected E2E web request to hit 'Server-3', got: %s", string(bodyWeb))
	}

	// 2. Test the "api-service" route
	reqAPI, _ := http.NewRequest("GET", e2eHostBase+"/", nil)
	reqAPI.Host = "api.example.com"
	respAPI, err := e2eClient.Do(reqAPI)
	if err != nil {
		t.Fatalf("E2E API request failed: %v", err)
	}
	bodyAPI, _ := io.ReadAll(respAPI.Body)
	respAPI.Body.Close()

	if !strings.Contains(string(bodyAPI), "Server-1") {
		t.Errorf("Expected E2E API request to hit 'Server-1', got: %s", string(bodyAPI))
	}
}

// TestE2E_AdminAPI tests that we can dynamically add a route and then hit it
func TestE2E_AdminAPI_DynamicRouting(t *testing.T) {
	// 1. Define a new route and backend
	// CHANGED: Prefixed types with 'lb.'
	newBackend := lb.BackendConfig{
		Addr:   "backend2:9002", // This is the *internal* Docker DNS name
		Weight: 1,
	}
	newRoute := lb.RouteConfig{
		Host:     "dynamic.example.com",
		Path:     "/",
		Strategy: "round-robin",
		Backends: []*lb.BackendConfig{&newBackend},
	}

	// 2. Add the route via the Admin API
	body, _ := json.Marshal(newRoute)
	reqAdd, _ := http.NewRequest("POST", e2eAdminAddr+"/api/v1/routes", bytes.NewBuffer(body))
	reqAdd.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	reqAdd.Header.Set("Content-Type", "application/json")

	respAdd, err := http.DefaultClient.Do(reqAdd)
	if err != nil {
		t.Fatalf("E2E admin POST request failed: %v", err)
	}
	if respAdd.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(respAdd.Body)
		t.Fatalf("Expected status 201 Created, got %d. Body: %s", respAdd.StatusCode, string(body))
	}
	respAdd.Body.Close()

	// Give the LB a moment to hot-reload
	time.Sleep(500 * time.Millisecond)

	// 3. Test the new dynamic route
	reqTest, _ := http.NewRequest("GET", e2eHostBase+"/", nil)
	reqTest.Host = "dynamic.example.com"
	respTest, err := e2eClient.Do(reqTest)
	if err != nil {
		t.Fatalf("E2E dynamic route request failed: %v", err)
	}
	bodyTest, _ := io.ReadAll(respTest.Body)
	respTest.Body.Close()

	if !strings.Contains(string(bodyTest), "Server-2") {
		t.Errorf("Expected E2E dynamic route to hit 'Server-2', got: %s", string(bodyTest))
	}
}
