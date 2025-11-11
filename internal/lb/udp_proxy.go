package lb

import (
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/sony/gobreaker"
)

const (
	udpSessionTimeout = 5 * time.Minute // How long a UDP "session" stays alive
	udpCleanup        = 1 * time.Minute // How often to scan for expired sessions
)

// UDPFlow represents a "session" mapping a client to a backend
type UDPFlow struct {
	clientAddr  *net.UDPAddr
	backendConn net.Conn // A connection to the chosen backend
}

// UDPProxy manages a UDP listener and its sessions
type UDPProxy struct {
	listener     *net.UDPConn
	pool         *BackendPool
	listenerName string
	sessionCache *cache.Cache // Manages UDP "sessions"
	stopChan     chan struct{}
	wg           sync.WaitGroup
}

// NewUDPProxy creates a new, un-started UDP proxy
func NewUDPProxy(listenerCfg *ListenerConfig, lb *LoadBalancer) (*UDPProxy, error) {
	pool, ok := lb.l4Pools[listenerCfg.Name]
	if !ok {
		return nil, errors.New("no L4 pool found for UDP listener: " + listenerCfg.Name)
	}

	addr, err := net.ResolveUDPAddr("udp", listenerCfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	// Create a cache with an eviction callback to close backend connections
	sessionCache := cache.New(udpSessionTimeout, udpCleanup)
	sessionCache.OnEvicted(func(key string, i interface{}) {
		if flow, ok := i.(*UDPFlow); ok {
			slog.Debug("Closing expired UDP backend connection", "client", key, "backend", flow.backendConn.RemoteAddr())
			if err := flow.backendConn.Close(); err != nil {
				slog.Warn("Failed to close UDP backend connection on eviction", "client", key, "error", err)
			}
		}
	})

	slog.Info("UDP listener configured", "name", listenerCfg.Name, "addr", listenerCfg.ListenAddr)
	return &UDPProxy{
		listener:     listener,
		pool:         pool,
		listenerName: listenerCfg.Name,
		sessionCache: sessionCache,
		stopChan:     make(chan struct{}),
	}, nil
}

// Run starts the listener's read loop
func (p *UDPProxy) Run() {
	slog.Info("UDP listener running", "name", p.listenerName, "addr", p.listener.LocalAddr())
	p.wg.Add(1)
	defer p.wg.Done()

	buffer := make([]byte, 1500) // Standard MTU

	for {
		select {
		case <-p.stopChan:
			slog.Info("UDP listener shutting down", "name", p.listenerName)
			return
		default:
			// Read a packet from the client
			if err := p.listener.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
				slog.Warn("Failed to set UDP read deadline", "name", p.listenerName, "error", err)
				return // A real error, stop the loop
			}
			n, clientAddr, err := p.listener.ReadFromUDP(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue // Just a timeout, loop again
				}
				slog.Warn("Failed to read from UDP", "name", p.listenerName, "error", err)
				return // A real error, stop the loop
			}

			// Handle the packet
			go p.handleUDPPacket(clientAddr, buffer[:n])
		}
	}
}

// Shutdown gracefully stops the UDP proxy
func (p *UDPProxy) Shutdown() {
	slog.Info("Closing UDP listener", "name", p.listenerName)
	close(p.stopChan) // Signal the Run() loop to stop
	if err := p.listener.Close(); err != nil {
		slog.Warn("Failed to close UDP listener", "name", p.listenerName, "error", err)
	}
	p.sessionCache.Flush() // This will trigger OnEvicted for all flows
}

// Wait blocks until the Run() loop exits
func (p *UDPProxy) Wait() {
	p.wg.Wait()
	slog.Info("UDP listener fully shut down", "name", p.listenerName)
}

func (p *UDPProxy) handleUDPPacket(clientAddr *net.UDPAddr, data []byte) {
	clientKey := clientAddr.String()

	// Check if a session already exists
	if flowInterface, found := p.sessionCache.Get(clientKey); found {
		flow := flowInterface.(*UDPFlow)
		// Session found, forward packet to the known backend
		_, err := flow.backendConn.Write(data)
		if err != nil {
			slog.Warn("Failed to write to UDP backend", "backend", flow.backendConn.RemoteAddr(), "error", err)
		}
		// Reset the session timeout
		p.sessionCache.Set(clientKey, flow, cache.DefaultExpiration)
		return
	}

	// --- No session found, create one ---
	logger := slog.Default().With(
		"protocol", "udp",
		"listener", p.listenerName,
		"client_ip", clientKey,
	)

	backend := p.pool.GetNextBackend(nil, clientKey) // nil *http.Request
	if backend == nil {
		logger.Warn("No healthy backend found for UDP connection")
		return
	}

	logger = logger.With("backend_addr", backend.Addr)

	// We must check if the circuit breaker is enabled for this backend
	if backend.cb != nil {
		_, err := backend.cb.Execute(func() (interface{}, error) {
			return p.proxyUdp(logger, backend, clientAddr, data)
		})
		if err != nil {
			if errors.Is(err, gobreaker.ErrOpenState) {
				logger.Warn("UDP flow blocked by circuit breaker", "state", backend.cb.State().String())
			} else {
				logger.Warn("Circuit breaker failure for UDP", "error", err)
			}
		}
	} else {
		// No circuit breaker, just proxy
		if _, err := p.proxyUdp(logger, backend, clientAddr, data); err != nil {
			logger.Warn("UDP proxy error", "error", err)
		}
	}
}

// proxyUdp contains the logic for creating a new UDP flow
func (p *UDPProxy) proxyUdp(logger *slog.Logger, backend *Backend, clientAddr *net.UDPAddr, data []byte) (interface{}, error) {
	// Dial the backend over UDP
	backendConn, err := net.Dial("udp", backend.Addr)
	if err != nil {
		logger.Warn("Failed to connect to UDP backend", "error", err)
		return nil, err
	}

	// Create the new flow
	flow := &UDPFlow{
		clientAddr:  clientAddr,
		backendConn: backendConn,
	}

	// Start a new goroutine to listen for responses from this backend
	p.wg.Add(1)
	go p.listenForBackend(flow, logger)

	// Add the new flow to the cache
	p.sessionCache.Set(clientAddr.String(), flow, cache.DefaultExpiration)
	logger.Info("Proxying new UDP flow")

	// Increment connection count (for metrics/strategy)
	backend.IncrementConnections()

	// Write the first packet
	_, err = backendConn.Write(data)
	return nil, err
}

// listenForBackend runs in a goroutine, reading from a backend and writing to the client
func (p *UDPProxy) listenForBackend(flow *UDPFlow, logger *slog.Logger) {
	defer p.wg.Done()
	defer func() {
		if err := flow.backendConn.Close(); err != nil {
			slog.Warn("Failed to close UDP backend connection", "error", err)
		}
	}()

	// Find the backend in the pool to decrement its connection count later
	var backend *Backend
	for _, b := range p.pool.backends {
		if b.Addr == flow.backendConn.RemoteAddr().String() {
			backend = b
			break
		}
	}
	if backend != nil {
		defer backend.DecrementConnections()
	}

	buffer := make([]byte, 1500)
	for {
		// Set a deadline to check if the session is still valid
		if err := flow.backendConn.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
			slog.Warn("Failed to set UDP backend read deadline", "backend", flow.backendConn.RemoteAddr(), "error", err)
			return // A real error
		}
		n, err := flow.backendConn.Read(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Check if the session was evicted
				if _, found := p.sessionCache.Get(flow.clientAddr.String()); !found {
					logger.Debug("UDP session expired, closing backend read loop", "client", flow.clientAddr)
					return // Session is gone, exit goroutine
				}
				continue // Session is still valid, continue reading
			}
			logger.Warn("Error reading from UDP backend", "backend", flow.backendConn.RemoteAddr(), "error", err)
			return // A real error
		}

		// Got a packet from the backend, send it to the client
		_, err = p.listener.WriteToUDP(buffer[:n], flow.clientAddr)
		if err != nil {
			logger.Warn("Failed to write UDP packet to client", "client", flow.clientAddr, "error", err)
		}
	}
}
