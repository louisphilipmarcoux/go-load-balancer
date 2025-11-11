package backend

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
)

// RunServer (HTTP Server)
func RunServer(port, serverID string) (net.Listener, error) {
	log.Printf("Starting backend server %s on port %s\n", serverID, port)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realIP := r.Header.Get("X-Forwarded-For")
		host := r.Header.Get("X-Forwarded-Host")
		proto := r.Header.Get("X-Forwarded-Proto")

		log.Printf(
			"[Server %s] Received request. LB-IP: %s, Client-IP: %s, Host: %s, Proto: %s",
			serverID,
			r.RemoteAddr,
			realIP,
			host,
			proto,
		)

		if _, err := fmt.Fprintf(w, "Hello from backend server: %s\n", serverID); err != nil {
			log.Printf("Warning: failed to write response: %v", err)
		}
	})

	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return nil, err
	}

	go func() {
		err := http.Serve(listener, handler)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	return listener, nil
}

// RunUDPServer starts a simple UDP echo server
func RunUDPServer(port, serverID string) (net.PacketConn, error) {
	log.Printf("Starting UDP backend server %s on port %s\n", serverID, port)
	addr, err := net.ResolveUDPAddr("udp", ":"+port)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	go func() {
		buffer := make([]byte, 1500)
		for {
			n, clientAddr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Printf("Failed to read from UDP: %v", err)
				}
				return // Stop loop on error
			}

			log.Printf("[UDP Server %s] Received packet from %s: %s", serverID, clientAddr, string(buffer[:n]))

			// Echo the message back with the server ID
			resp := []byte(fmt.Sprintf("Hello from UDP server: %s", serverID))
			if _, err := conn.WriteToUDP(resp, clientAddr); err != nil {
				log.Printf("Failed to write UDP response: %v", err)
			}
		}
	}()

	return conn, nil
}
