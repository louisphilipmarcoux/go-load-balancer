package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv" // NEW
	"strings" // NEW

	"github.com/gorilla/mux"
)

// NEW: adminAuthMiddleware checks for the bearer token
func adminAuthMiddleware(lb *LoadBalancer) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			lb.lock.RLock()
			expectedToken := lb.cfg.AdminToken
			lb.lock.RUnlock()

			if expectedToken == "" {
				slog.Warn("Admin auth is disabled (no ADMIN_TOKEN in config)")
				next.ServeHTTP(w, r)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				respondWithError(w, http.StatusUnauthorized, "Authorization header required")
				return
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == authHeader {
				respondWithError(w, http.StatusUnauthorized, "Invalid authorization format. Expected: Bearer <token>")
				return
			}
			if token != expectedToken {
				slog.Warn("Invalid admin token received", "remote_addr", r.RemoteAddr)
				respondWithError(w, http.StatusUnauthorized, "Invalid token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func StartAdminServer(lb *LoadBalancer) *http.Server {
	if lb.cfg.AdminAddr == "" {
		slog.Info("Admin server is disabled (adminAddr not set)")
		return nil
	}
	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.Use(adminAuthMiddleware(lb))

	api.HandleFunc("/routes", getRoutesHandler(lb)).Methods("GET")
	api.HandleFunc("/routes", addRouteHandler(lb)).Methods("POST")
	api.HandleFunc("/routes/{index:[0-9]+}", getRouteHandler(lb)).Methods("GET")
	api.HandleFunc("/routes/{index:[0-9]+}", deleteRouteHandler(lb)).Methods("DELETE")
	api.HandleFunc("/routes/{index:[0-9]+}/backends", addBackendHandler(lb)).Methods("POST")
	api.HandleFunc("/routes/{index:[0-9]+}/backends/{host}", deleteBackendHandler(lb)).Methods("DELETE")
	api.HandleFunc("/routes/{index:[0-9]+}/backends/{host}", updateBackendHandler(lb)).Methods("PUT")

	adminServer := &http.Server{
		Addr:    lb.cfg.AdminAddr,
		Handler: r,
	}

	go func() {
		slog.Info("Admin API server listening", "addr", lb.cfg.AdminAddr)
		if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Admin server failed", "error", err)
		}
	}()
	return adminServer
}

func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, map[string]string{"error": message})
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		slog.Error("Error marshaling JSON for admin response", "error", err)
		http.Error(w, "Failed to marshal JSON response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	response = append(response, '\n')
	if _, err := w.Write(response); err != nil {
		slog.Warn("Error writing JSON response", "error", err)
	}
}

// --- Route Handlers (CHANGED to use index) ---

func getRoutesHandler(lb *LoadBalancer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lb.lock.RLock()
		cfg := lb.cfg
		lb.lock.RUnlock()
		respondWithJSON(w, http.StatusOK, cfg.Routes)
	}
}

func addRouteHandler(lb *LoadBalancer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var newRoute RouteConfig
		if err := json.NewDecoder(r.Body).Decode(&newRoute); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid JSON payload")
			return
		}
		if newRoute.Path == "" {
			respondWithError(w, http.StatusBadRequest, "Route 'path' is required")
			return
		}
		if err := lb.AddRoute(&newRoute); err != nil {
			respondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondWithJSON(w, http.StatusCreated, newRoute)
	}
}

// Helper to parse index from URL
func getIndex(r *http.Request) (int, error) {
	vars := mux.Vars(r)
	index, err := strconv.Atoi(vars["index"])
	if err != nil {
		return 0, errors.New("invalid route index")
	}
	return index, nil
}

func getRouteHandler(lb *LoadBalancer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		index, err := getIndex(r)
		if err != nil {
			respondWithError(w, http.StatusBadRequest, err.Error())
			return
		}

		lb.lock.RLock()
		defer lb.lock.RUnlock()

		route, err := lb.getRouteByIndex(index)
		if err != nil {
			respondWithError(w, http.StatusNotFound, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, route)
	}
}

func deleteRouteHandler(lb *LoadBalancer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		index, err := getIndex(r)
		if err != nil {
			respondWithError(w, http.StatusBadRequest, err.Error())
			return
		}

		if err := lb.DeleteRoute(index); err != nil {
			respondWithError(w, http.StatusNotFound, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

// --- Backend Handlers (CHANGED to use index) ---

func addBackendHandler(lb *LoadBalancer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		index, err := getIndex(r)
		if err != nil {
			respondWithError(w, http.StatusBadRequest, err.Error())
			return
		}

		var newBackend BackendConfig
		if err := json.NewDecoder(r.Body).Decode(&newBackend); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid JSON payload")
			return
		}
		if newBackend.Addr == "" {
			respondWithError(w, http.StatusBadRequest, "Backend 'addr' is required")
			return
		}

		if err := lb.AddBackendToRoute(index, &newBackend); err != nil {
			respondWithError(w, http.StatusNotFound, err.Error())
			return
		}
		respondWithJSON(w, http.StatusCreated, newBackend)
	}
}

func deleteBackendHandler(lb *LoadBalancer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		index, err := getIndex(r)
		if err != nil {
			respondWithError(w, http.StatusBadRequest, err.Error())
			return
		}
		vars := mux.Vars(r)
		host, _ := url.PathUnescape(vars["host"]) // e.g., localhost:9004

		if err := lb.DeleteBackendFromRoute(index, host); err != nil {
			respondWithError(w, http.StatusNotFound, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func updateBackendHandler(lb *LoadBalancer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		index, err := getIndex(r)
		if err != nil {
			respondWithError(w, http.StatusBadRequest, err.Error())
			return
		}
		vars := mux.Vars(r)
		host, _ := url.PathUnescape(vars["host"])

		var payload struct {
			Weight int `json:"weight"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid JSON payload")
			return
		}

		if err := lb.UpdateBackendWeightInRoute(index, host, payload.Weight); err != nil {
			respondWithError(w, http.StatusNotFound, err.Error())
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	}
}
