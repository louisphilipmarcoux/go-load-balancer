package lb

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouteMatches(t *testing.T) {
	// Define test cases
	testCases := []struct {
		name        string
		routeConfig *RouteConfig
		request     *http.Request
		shouldMatch bool
	}{
		{
			name:        "Simple path match",
			routeConfig: &RouteConfig{Path: "/api/"},
			request:     httptest.NewRequest("GET", "http://example.com/api/users", nil),
			shouldMatch: true,
		},
		{
			name:        "Path mismatch",
			routeConfig: &RouteConfig{Path: "/api/"},
			request:     httptest.NewRequest("GET", "http://example.com/web", nil),
			shouldMatch: false,
		},
		{
			name:        "Root path match",
			routeConfig: &RouteConfig{Path: "/"},
			request:     httptest.NewRequest("GET", "http://example.com/web", nil),
			shouldMatch: true,
		},
		{
			name:        "Host match",
			routeConfig: &RouteConfig{Host: "api.example.com"},
			request:     httptest.NewRequest("GET", "http://api.example.com/users", nil),
			shouldMatch: true,
		},
		{
			name:        "Host mismatch",
			routeConfig: &RouteConfig{Host: "api.example.com"},
			request:     httptest.NewRequest("GET", "http://web.example.com/users", nil),
			shouldMatch: false,
		},
		{
			name:        "Header match",
			routeConfig: &RouteConfig{Headers: map[string]string{"X-Role": "admin"}},
			request: func() *http.Request {
				req := httptest.NewRequest("GET", "http://example.com/", nil)
				req.Header.Set("X-Role", "admin")
				return req
			}(),
			shouldMatch: true,
		},
		{
			name:        "Header mismatch",
			routeConfig: &RouteConfig{Headers: map[string]string{"X-Role": "admin"}},
			request: func() *http.Request {
				req := httptest.NewRequest("GET", "http://example.com/", nil)
				req.Header.Set("X-Role", "user")
				return req
			}(),
			shouldMatch: false,
		},
		{
			name: "All rules match",
			routeConfig: &RouteConfig{
				Host:    "admin.example.com",
				Path:    "/v1/",
				Headers: map[string]string{"X-Auth": "true"},
			},
			request: func() *http.Request {
				req := httptest.NewRequest("GET", "http://admin.example.com/v1/delete", nil)
				req.Header.Set("X-Auth", "true")
				return req
			}(),
			shouldMatch: true,
		},
		{
			name: "One rule (host) fails",
			routeConfig: &RouteConfig{
				Host:    "admin.example.com",
				Path:    "/v1/",
				Headers: map[string]string{"X-Auth": "true"},
			},
			request: func() *http.Request {
				// Wrong host
				req := httptest.NewRequest("GET", "http://api.example.com/v1/delete", nil)
				req.Header.Set("X-Auth", "true")
				return req
			}(),
			shouldMatch: false,
		},
	}

	// Run test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			route := &Route{config: tc.routeConfig}
			matches := route.Matches(tc.request)
			if matches != tc.shouldMatch {
				t.Errorf("Expected match to be %v, but got %v", tc.shouldMatch, matches)
			}
		})
	}
}
